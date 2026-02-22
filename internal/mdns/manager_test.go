package mdns

import (
	"fmt"
	"testing"
	"time"

	"piccolod/internal/events"
)

func TestNewManager(t *testing.T) {
	manager := NewManager()

	// Test initialization
	if manager == nil {
		t.Fatal("NewManager() returned nil")
	}

	// Test machine ID generation
	if manager.machineID == "" {
		t.Error("machineID should not be empty")
	}

	if len(manager.machineID) != 6 {
		t.Errorf("machineID length = %v, want 6", len(manager.machineID))
	}

	// Test that baseName is now the specific name (piccolo-<machineId>)
	// This ensures only gateway leader serves piccolo.local
	expectedBaseName := "piccolo-" + manager.machineID
	if manager.hostname != expectedBaseName {
		t.Errorf("hostname = %v, want %v", manager.hostname, expectedBaseName)
	}

	if manager.port != 80 {
		t.Errorf("port = %v, want %v", manager.port, 80)
	}

	if manager.baseName != expectedBaseName {
		t.Errorf("baseName = %v, want %v", manager.baseName, expectedBaseName)
	}

	if manager.finalName != expectedBaseName {
		t.Errorf("finalName = %v, want %v", manager.finalName, expectedBaseName)
	}

	// Test security components initialization
	if manager.securityConfig == nil {
		t.Error("securityConfig should be initialized")
	}

	if manager.securityMetrics == nil {
		t.Error("securityMetrics should be initialized")
	}

	if manager.queryProcessor == nil {
		t.Error("queryProcessor should be initialized")
	}

	// Test resilience components initialization
	if manager.resilienceConfig == nil {
		t.Error("resilienceConfig should be initialized")
	}

	if manager.healthMonitor == nil {
		t.Error("healthMonitor should be initialized")
	}

	// Test collections initialization
	if manager.interfaces == nil {
		t.Error("interfaces map should be initialized")
	}

	if len(manager.interfaces) != 0 {
		t.Error("interfaces map should be empty initially")
	}

	// Test channel initialization
	if manager.stopCh == nil {
		t.Error("stopCh should be initialized")
	}
}

func TestManagerSecurityConfigDefaults(t *testing.T) {
	manager := NewManager()

	config := manager.securityConfig
	tests := []struct {
		name     string
		actual   interface{}
		expected interface{}
	}{
		{"MaxPacketSize", config.MaxPacketSize, 1500},
		{"MaxResponseSize", config.MaxResponseSize, 512},
		{"MaxConcurrentQueries", config.MaxConcurrentQueries, 50},
		{"QueryTimeout", config.QueryTimeout, time.Second * 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.actual != tt.expected {
				t.Errorf("%s = %v, want %v", tt.name, tt.actual, tt.expected)
			}
		})
	}
}

func TestManagerResilienceConfigDefaults(t *testing.T) {
	manager := NewManager()

	config := manager.resilienceConfig
	tests := []struct {
		name     string
		actual   interface{}
		expected interface{}
	}{
		{"MaxRetries", config.MaxRetries, 3},
		{"InitialBackoff", config.InitialBackoff, time.Second * 5},
		{"MaxBackoff", config.MaxBackoff, time.Minute * 5},
		{"BackoffMultiplier", config.BackoffMultiplier, 2.0},
		{"HealthCheckInterval", config.HealthCheckInterval, time.Second * 30},
		{"RecoveryCheckInterval", config.RecoveryCheckInterval, time.Second * 15},
		{"MaxFailureRate", config.MaxFailureRate, 0.3},
		{"MinHealthScore", config.MinHealthScore, 0.7},
		{"RecoveryTimeout", config.RecoveryTimeout, time.Minute * 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.actual != tt.expected {
				t.Errorf("%s = %v, want %v", tt.name, tt.actual, tt.expected)
			}
		})
	}
}

func TestManagerHealthMonitorDefaults(t *testing.T) {
	manager := NewManager()

	monitor := manager.healthMonitor

	if monitor.OverallHealth != 1.0 {
		t.Errorf("OverallHealth = %v, want %v", monitor.OverallHealth, 1.0)
	}

	if monitor.InterfaceHealth == nil {
		t.Error("InterfaceHealth map should be initialized")
	}

	if len(monitor.InterfaceHealth) != 0 {
		t.Error("InterfaceHealth map should be empty initially")
	}

	if !assertTimestamp(monitor.LastHealthCheck, time.Second) {
		t.Error("LastHealthCheck should be recent")
	}

	if monitor.RecoveryActive {
		t.Error("RecoveryActive should be false initially")
	}

	if monitor.SystemErrors != 0 {
		t.Errorf("SystemErrors = %v, want %v", monitor.SystemErrors, 0)
	}

	if monitor.RecoveryAttempts != 0 {
		t.Errorf("RecoveryAttempts = %v, want %v", monitor.RecoveryAttempts, 0)
	}
}

func TestManagerQueryProcessorDefaults(t *testing.T) {
	manager := NewManager()

	processor := manager.queryProcessor

	if processor.semaphore == nil {
		t.Error("queryProcessor.semaphore should be initialized")
	}

	// Test semaphore capacity matches security config
	expectedCapacity := manager.securityConfig.MaxConcurrentQueries
	semaphoreCapacity := cap(processor.semaphore)

	if semaphoreCapacity != expectedCapacity {
		t.Errorf("semaphore capacity = %v, want %v", semaphoreCapacity, expectedCapacity)
	}

	if processor.activeCount != 0 {
		t.Errorf("activeCount = %v, want %v", processor.activeCount, 0)
	}
}

func TestGetMachineIDDeterministic(t *testing.T) {
	// Test that getMachineID returns consistent results
	id1 := getMachineID()
	id2 := getMachineID()

	if id1 != id2 {
		t.Errorf("getMachineID() should be deterministic: got %s and %s", id1, id2)
	}

	if len(id1) != 6 {
		t.Errorf("getMachineID() length = %v, want 6", len(id1))
	}

	// Validate it's a hex string
	for _, char := range id1 {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			t.Errorf("getMachineID() should return hex string, got %s", id1)
			break
		}
	}
}

func TestManagerStop(t *testing.T) {
	manager := NewManager()

	// Test stop on unstarted manager (should not panic)
	err := manager.Stop()
	if err != nil {
		t.Errorf("Stop() on unstarted manager should not error, got: %v", err)
	}

	// Test stop channel is closed
	select {
	case <-manager.stopCh:
		// Channel is closed, this is expected
	case <-time.After(time.Millisecond * 100):
		t.Error("Stop() should close the stopCh channel")
	}
}

func TestManagerInterfaceMapOperations(t *testing.T) {
	manager := NewManager()

	// Test adding interface state
	testState := createMockInterfaceState("test0", true, true)

	manager.mutex.Lock()
	manager.interfaces["test0"] = testState
	manager.mutex.Unlock()

	// Test retrieval
	manager.mutex.RLock()
	retrieved, exists := manager.interfaces["test0"]
	count := len(manager.interfaces)
	manager.mutex.RUnlock()

	if !exists {
		t.Error("Interface should exist after adding")
	}

	if retrieved != testState {
		t.Error("Retrieved interface state should match added state")
	}

	if count != 1 {
		t.Errorf("Interface count = %v, want %v", count, 1)
	}

	// Test removal
	manager.mutex.Lock()
	delete(manager.interfaces, "test0")
	manager.mutex.Unlock()

	manager.mutex.RLock()
	_, exists = manager.interfaces["test0"]
	count = len(manager.interfaces)
	manager.mutex.RUnlock()

	if exists {
		t.Error("Interface should not exist after removal")
	}

	if count != 0 {
		t.Errorf("Interface count after removal = %v, want %v", count, 0)
	}
}

func TestManagerConcurrentAccess(t *testing.T) {
	manager := NewManager()

	// Test concurrent read/write access to interfaces map
	done := make(chan bool, 2)

	// Writer goroutine
	go func() {
		for i := 0; i < 10; i++ {
			state := createMockInterfaceState(fmt.Sprintf("test%d", i), true, false)
			manager.mutex.Lock()
			manager.interfaces[fmt.Sprintf("test%d", i)] = state
			manager.mutex.Unlock()
			time.Sleep(time.Microsecond)
		}
		done <- true
	}()

	// Reader goroutine
	go func() {
		for i := 0; i < 10; i++ {
			manager.mutex.RLock()
			_ = len(manager.interfaces)
			manager.mutex.RUnlock()
			time.Sleep(time.Microsecond)
		}
		done <- true
	}()

	// Wait for both goroutines
	<-done
	<-done

	// Verify final state
	manager.mutex.RLock()
	finalCount := len(manager.interfaces)
	manager.mutex.RUnlock()

	if finalCount != 10 {
		t.Errorf("Final interface count = %v, want %v", finalCount, 10)
	}
}

// TestGoroutineDeadlockRegression ensures Bug #1 (goroutine deadlock) stays fixed
func TestGoroutineDeadlockRegression(t *testing.T) {
	t.Log("=== REGRESSION TEST: Goroutine Deadlock Prevention ===")

	// This test ensures that the manager can start and stop cleanly without deadlocks
	// Previously, missing defer m.wg.Done() caused hangs on manager.Stop()

	manager := newStubbedManager(t, defaultStubNetworkEnv())

	err := manager.Start()
	if err != nil {
		t.Logf("Manager start failed (may be expected in test env): %v", err)
		return
	}

	// Give it time to start all goroutines
	time.Sleep(100 * time.Millisecond)

	// This should complete within reasonable time without hanging
	done := make(chan struct{})
	go func() {
		manager.Stop()
		close(done)
	}()

	// Timeout test - should not hang
	select {
	case <-done:
		t.Log("Manager stopped cleanly - no deadlock")
	case <-time.After(5 * time.Second):
		t.Error("REGRESSION: Manager.Stop() hung - deadlock detected!")
	}
}

func TestHandleServiceEndpointsChanged_AddLabels(t *testing.T) {
	manager := NewManager()

	payload := events.ServiceEndpointsChanged{
		App: "myapp",
		Added: []events.ServiceEndpointInfo{
			{App: "myapp", Name: "web", DerivedHostLabel: "myapp"},
			{App: "myapp", Name: "api", DerivedHostLabel: "api-myapp"},
		},
		Removed: nil,
	}

	manager.handleServiceEndpointsChanged(payload)

	manager.appHostLabelsMu.RLock()
	labels := manager.appHostLabels["myapp"]
	manager.appHostLabelsMu.RUnlock()

	if len(labels) != 2 {
		t.Fatalf("expected 2 labels, got %d", len(labels))
	}

	// Check that all labels are present
	labelSet := make(map[string]bool)
	for _, l := range labels {
		labelSet[l] = true
	}
	if !labelSet["myapp"] || !labelSet["api-myapp"] {
		t.Errorf("expected labels [myapp, api-myapp], got %v", labels)
	}
}

func TestHandleServiceEndpointsChanged_RemoveLabels(t *testing.T) {
	manager := NewManager()

	// Pre-populate with labels
	manager.appHostLabelsMu.Lock()
	manager.appHostLabels["myapp"] = []string{"myapp", "api-myapp", "db-myapp"}
	manager.appHostLabelsMu.Unlock()

	// Remove one label
	payload := events.ServiceEndpointsChanged{
		App: "myapp",
		Added: nil,
		Removed: []events.ServiceEndpointInfo{
			{App: "myapp", Name: "db", DerivedHostLabel: "db-myapp"},
		},
	}

	manager.handleServiceEndpointsChanged(payload)

	manager.appHostLabelsMu.RLock()
	labels := manager.appHostLabels["myapp"]
	manager.appHostLabelsMu.RUnlock()

	if len(labels) != 2 {
		t.Fatalf("expected 2 labels after removal, got %d", len(labels))
	}

	// Check that removed label is gone
	for _, l := range labels {
		if l == "db-myapp" {
			t.Error("db-myapp should have been removed")
		}
	}
}

func TestHandleServiceEndpointsChanged_MergeDelta(t *testing.T) {
	manager := NewManager()

	// Pre-populate with existing labels
	manager.appHostLabelsMu.Lock()
	manager.appHostLabels["myapp"] = []string{"myapp", "api-myapp"}
	manager.appHostLabelsMu.Unlock()

	// Reconcile: remove api, add db
	payload := events.ServiceEndpointsChanged{
		App: "myapp",
		Added: []events.ServiceEndpointInfo{
			{App: "myapp", Name: "db", DerivedHostLabel: "db-myapp"},
		},
		Removed: []events.ServiceEndpointInfo{
			{App: "myapp", Name: "api", DerivedHostLabel: "api-myapp"},
		},
	}

	manager.handleServiceEndpointsChanged(payload)

	manager.appHostLabelsMu.RLock()
	labels := manager.appHostLabels["myapp"]
	manager.appHostLabelsMu.RUnlock()

	if len(labels) != 2 {
		t.Fatalf("expected 2 labels after merge, got %d: %v", len(labels), labels)
	}

	labelSet := make(map[string]bool)
	for _, l := range labels {
		labelSet[l] = true
	}

	if !labelSet["myapp"] {
		t.Error("myapp label should be preserved")
	}
	if labelSet["api-myapp"] {
		t.Error("api-myapp should have been removed")
	}
	if !labelSet["db-myapp"] {
		t.Error("db-myapp should have been added")
	}
}

func TestHandleServiceEndpointsChanged_EmptyAppIgnored(t *testing.T) {
	manager := NewManager()

	// Payload with empty app should be ignored
	payload := events.ServiceEndpointsChanged{
		App: "",
		Added: []events.ServiceEndpointInfo{
			{App: "", Name: "web", DerivedHostLabel: "test"},
		},
	}

	manager.handleServiceEndpointsChanged(payload)

	manager.appHostLabelsMu.RLock()
	count := len(manager.appHostLabels)
	manager.appHostLabelsMu.RUnlock()

	if count != 0 {
		t.Errorf("expected no labels for empty app, got %d entries", count)
	}
}

func TestDebouncedAnnouncement(t *testing.T) {
	manager := NewManager()

	// Send multiple rapid events
	for i := 0; i < 10; i++ {
		payload := events.ServiceEndpointsChanged{
			App: fmt.Sprintf("app%d", i),
			Added: []events.ServiceEndpointInfo{
				{App: fmt.Sprintf("app%d", i), Name: "web", DerivedHostLabel: fmt.Sprintf("app%d", i)},
			},
		}
		manager.handleServiceEndpointsChanged(payload)
	}

	// Verify a timer is pending
	manager.announceDebounceMu.Lock()
	pending := manager.announcePending
	hasTimer := manager.announceDebounceTimer != nil
	manager.announceDebounceMu.Unlock()

	if !pending {
		t.Error("expected announcePending to be true after rapid events")
	}
	if !hasTimer {
		t.Error("expected debounce timer to be set")
	}

	// Verify internal state was accumulated correctly
	manager.appHostLabelsMu.RLock()
	appCount := len(manager.appHostLabels)
	manager.appHostLabelsMu.RUnlock()

	if appCount != 10 {
		t.Errorf("expected 10 apps tracked, got %d", appCount)
	}

	// Stop the observer to cancel the timer (cleanup)
	manager.StopServiceEndpointsObserver()

	// Verify timer was cancelled
	manager.announceDebounceMu.Lock()
	pendingAfter := manager.announcePending
	timerAfter := manager.announceDebounceTimer
	manager.announceDebounceMu.Unlock()

	if pendingAfter {
		t.Error("expected announcePending to be false after stop")
	}
	if timerAfter != nil {
		t.Error("expected timer to be nil after stop")
	}
}
