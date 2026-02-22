package mdns

import (
	"testing"
	"time"
)

func TestInterfaceState(t *testing.T) {
	tests := []struct {
		name     string
		hasIPv4  bool
		hasIPv6  bool
		wantIPv4 bool
		wantIPv6 bool
	}{
		{
			name:     "IPv4 only",
			hasIPv4:  true,
			hasIPv6:  false,
			wantIPv4: true,
			wantIPv6: false,
		},
		{
			name:     "IPv6 only",
			hasIPv4:  false,
			hasIPv6:  true,
			wantIPv4: false,
			wantIPv6: true,
		},
		{
			name:     "Dual stack",
			hasIPv4:  true,
			hasIPv6:  true,
			wantIPv4: true,
			wantIPv6: true,
		},
		{
			name:     "No IP addresses",
			hasIPv4:  false,
			hasIPv6:  false,
			wantIPv4: false,
			wantIPv6: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := createMockInterfaceState("test0", tt.hasIPv4, tt.hasIPv6)

			if state.HasIPv4 != tt.wantIPv4 {
				t.Errorf("HasIPv4 = %v, want %v", state.HasIPv4, tt.wantIPv4)
			}

			if state.HasIPv6 != tt.wantIPv6 {
				t.Errorf("HasIPv6 = %v, want %v", state.HasIPv6, tt.wantIPv6)
			}

			if tt.wantIPv4 && state.IPv4 == nil {
				t.Error("Expected IPv4 address but got nil")
			}

			if tt.wantIPv6 && state.IPv6 == nil {
				t.Error("Expected IPv6 address but got nil")
			}

			// Verify initial state
			if !state.Active {
				t.Error("Expected interface to be active initially")
			}

			if state.HealthScore != 1.0 {
				t.Errorf("Expected initial health score of 1.0, got %f", state.HealthScore)
			}

			if !assertTimestamp(state.LastSeen, time.Second) {
				t.Error("LastSeen timestamp should be recent")
			}
		})
	}
}

func TestSecurityConfig(t *testing.T) {
	config := createMockSecurityConfig()

	// Test configuration validation
	tests := []struct {
		name     string
		field    string
		getValue func() interface{}
		isValid  func(interface{}) bool
	}{
		{
			name:     "MaxPacketSize reasonable",
			field:    "MaxPacketSize",
			getValue: func() interface{} { return config.MaxPacketSize },
			isValid:  func(v interface{}) bool { return v.(int) > 512 && v.(int) <= 65535 },
		},
		{
			name:     "QueryTimeout positive",
			field:    "QueryTimeout",
			getValue: func() interface{} { return config.QueryTimeout },
			isValid:  func(v interface{}) bool { return v.(time.Duration) > 0 },
		},
		{
			name:     "MaxConcurrentQueries positive",
			field:    "MaxConcurrentQueries",
			getValue: func() interface{} { return config.MaxConcurrentQueries },
			isValid:  func(v interface{}) bool { return v.(int) > 0 },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value := tt.getValue()
			if !tt.isValid(value) {
				t.Errorf("Invalid %s: %v", tt.field, value)
			}
		})
	}
}

func TestResilienceConfig(t *testing.T) {
	config := createMockResilienceConfig()

	// Test configuration validation
	tests := []struct {
		name     string
		field    string
		getValue func() interface{}
		isValid  func(interface{}) bool
	}{
		{
			name:     "MaxRetries positive",
			field:    "MaxRetries",
			getValue: func() interface{} { return config.MaxRetries },
			isValid:  func(v interface{}) bool { return v.(int) > 0 },
		},
		{
			name:     "BackoffMultiplier greater than 1",
			field:    "BackoffMultiplier",
			getValue: func() interface{} { return config.BackoffMultiplier },
			isValid:  func(v interface{}) bool { return v.(float64) > 1.0 },
		},
		{
			name:     "MaxFailureRate between 0 and 1",
			field:    "MaxFailureRate",
			getValue: func() interface{} { return config.MaxFailureRate },
			isValid:  func(v interface{}) bool { return v.(float64) > 0.0 && v.(float64) <= 1.0 },
		},
		{
			name:     "MinHealthScore between 0 and 1",
			field:    "MinHealthScore",
			getValue: func() interface{} { return config.MinHealthScore },
			isValid:  func(v interface{}) bool { return v.(float64) > 0.0 && v.(float64) <= 1.0 },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value := tt.getValue()
			if !tt.isValid(value) {
				t.Errorf("Invalid %s: %v", tt.field, value)
			}
		})
	}
}

func TestSecurityMetrics(t *testing.T) {
	metrics := &SecurityMetrics{
		TotalQueries:     100,
		MalformedPackets: 2,
		LargePackets:     1,
	}

	if metrics.TotalQueries != 100 {
		t.Errorf("TotalQueries = %v, want %v", metrics.TotalQueries, 100)
	}

	if metrics.MalformedPackets != 2 {
		t.Errorf("MalformedPackets = %v, want %v", metrics.MalformedPackets, 2)
	}

	if metrics.LargePackets != 1 {
		t.Errorf("LargePackets = %v, want %v", metrics.LargePackets, 1)
	}

	// Test individual counters don't exceed total queries
	individualBlocked := metrics.MalformedPackets + metrics.LargePackets
	if individualBlocked > metrics.TotalQueries {
		t.Error("Sum of individual blocked counters should not exceed TotalQueries")
	}
}

func TestHealthMonitor(t *testing.T) {
	monitor := &HealthMonitor{
		OverallHealth:    0.8,
		InterfaceHealth:  make(map[string]float64),
		LastHealthCheck:  time.Now(),
		RecoveryActive:   false,
		SystemErrors:     0,
		RecoveryAttempts: 0,
	}

	// Test initial health state
	if monitor.OverallHealth < 0.0 || monitor.OverallHealth > 1.0 {
		t.Errorf("OverallHealth should be between 0.0 and 1.0, got %f", monitor.OverallHealth)
	}

	// Test interface health tracking
	interfaceNames := []string{"eth0", "wlan0", "lo"}
	for i, name := range interfaceNames {
		health := float64(i+1) * 0.3 // 0.3, 0.6, 0.9
		if health > 1.0 {
			health = 1.0
		}
		monitor.InterfaceHealth[name] = health
	}

	if len(monitor.InterfaceHealth) != len(interfaceNames) {
		t.Errorf("Expected %d interface health entries, got %d",
			len(interfaceNames), len(monitor.InterfaceHealth))
	}

	// Verify all health values are within valid range
	for name, health := range monitor.InterfaceHealth {
		if health < 0.0 || health > 1.0 {
			t.Errorf("Interface %s health should be between 0.0 and 1.0, got %f", name, health)
		}
	}
}
