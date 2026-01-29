package services

import (
	"testing"
	"time"
)

func TestStatusWorseThan(t *testing.T) {
	tests := []struct {
		name string
		a    ListenerHealthStatus
		b    ListenerHealthStatus
		want bool
	}{
		{"error worse than ok", ListenerHealthError, ListenerHealthOK, true},
		{"error worse than degraded", ListenerHealthError, ListenerHealthDegraded, true},
		{"error worse than recovering", ListenerHealthError, ListenerHealthRecovering, true},
		{"error not worse than error", ListenerHealthError, ListenerHealthError, false},
		{"recovering worse than ok", ListenerHealthRecovering, ListenerHealthOK, true},
		{"recovering worse than degraded", ListenerHealthRecovering, ListenerHealthDegraded, true},
		{"recovering not worse than error", ListenerHealthRecovering, ListenerHealthError, false},
		{"degraded worse than ok", ListenerHealthDegraded, ListenerHealthOK, true},
		{"degraded not worse than recovering", ListenerHealthDegraded, ListenerHealthRecovering, false},
		{"ok not worse than anything", ListenerHealthOK, ListenerHealthDegraded, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := statusWorseThan(tt.a, tt.b); got != tt.want {
				t.Errorf("statusWorseThan(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestDeriveListenerHealth_BackendOnly(t *testing.T) {
	tests := []struct {
		name       string
		backendOK  bool
		wantStatus ListenerHealthStatus
		wantCode   string
	}{
		{
			name:       "backend OK no certs",
			backendOK:  true,
			wantStatus: ListenerHealthOK,
			wantCode:   "ok",
		},
		{
			name:       "backend down no certs",
			backendOK:  false,
			wantStatus: ListenerHealthError,
			wantCode:   "backend_unreachable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			health := DeriveListenerHealth(nil, nil, tt.backendOK, nil)
			if health.Status != tt.wantStatus {
				t.Errorf("Status = %v, want %v", health.Status, tt.wantStatus)
			}
			if health.ReasonCode != tt.wantCode {
				t.Errorf("ReasonCode = %v, want %v", health.ReasonCode, tt.wantCode)
			}
		})
	}
}

func TestDeriveListenerHealth_CertStatuses(t *testing.T) {
	expiresOK := time.Now().Add(30 * 24 * time.Hour)
	expiresSoon := time.Now().Add(3 * 24 * time.Hour)
	retryAt := time.Now().Add(1 * time.Hour)

	tests := []struct {
		name       string
		certIDs    []string
		certs      map[string]*CertificateInfo
		wantStatus ListenerHealthStatus
		wantCode   string
	}{
		{
			name:    "cert pending (nil cert)",
			certIDs: []string{"wildcard"},
			certs:   nil,
			wantStatus: ListenerHealthRecovering,
			wantCode:   "cert_pending",
		},
		{
			name:    "cert OK",
			certIDs: []string{"wildcard"},
			certs: map[string]*CertificateInfo{
				"wildcard": {Status: "ok", ExpiresAt: &expiresOK},
			},
			wantStatus: ListenerHealthOK,
			wantCode:   "ok",
		},
		{
			name:    "cert expiring soon",
			certIDs: []string{"wildcard"},
			certs: map[string]*CertificateInfo{
				"wildcard": {Status: "ok", ExpiresAt: &expiresSoon},
			},
			wantStatus: ListenerHealthDegraded,
			wantCode:   "cert_expiring_soon",
		},
		{
			name:    "cert pending status",
			certIDs: []string{"wildcard"},
			certs: map[string]*CertificateInfo{
				"wildcard": {Status: "pending"},
			},
			wantStatus: ListenerHealthRecovering,
			wantCode:   "cert_pending",
		},
		{
			name:    "cert error - transient",
			certIDs: []string{"wildcard"},
			certs: map[string]*CertificateInfo{
				"wildcard": {
					Status:       "error",
					FailureClass: FailureClassTransient,
					RetryAt:      &retryAt,
				},
			},
			wantStatus: ListenerHealthRecovering,
			wantCode:   "cert_retry_scheduled",
		},
		{
			name:    "cert error - rate limited",
			certIDs: []string{"wildcard"},
			certs: map[string]*CertificateInfo{
				"wildcard": {
					Status:       "error",
					FailureClass: FailureClassRateLimited,
					RetryAt:      &retryAt,
				},
			},
			wantStatus: ListenerHealthError,
			wantCode:   "cert_rate_limited",
		},
		{
			name:    "cert error - config error",
			certIDs: []string{"wildcard"},
			certs: map[string]*CertificateInfo{
				"wildcard": {
					Status:       "error",
					FailureClass: FailureClassConfigError,
					FailureCode:  "cert_dns_error",
					RetryAt:      &retryAt,
				},
			},
			wantStatus: ListenerHealthError,
			wantCode:   "cert_dns_error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			health := DeriveListenerHealth(tt.certIDs, tt.certs, true, nil)
			if health.Status != tt.wantStatus {
				t.Errorf("Status = %v, want %v", health.Status, tt.wantStatus)
			}
			if health.ReasonCode != tt.wantCode {
				t.Errorf("ReasonCode = %v, want %v", health.ReasonCode, tt.wantCode)
			}
		})
	}
}

func TestDeriveListenerHealth_ActionRequired(t *testing.T) {
	retryAt := time.Now().Add(1 * time.Hour)

	// Config error should set ActionRequired=true
	health := DeriveListenerHealth(
		[]string{"wildcard"},
		map[string]*CertificateInfo{
			"wildcard": {
				Status:       "error",
				FailureClass: FailureClassConfigError,
				FailureCode:  "cert_dns_error",
				RetryAt:      &retryAt,
			},
		},
		true,
		nil,
	)

	if !health.ActionRequired {
		t.Error("ActionRequired should be true for config errors")
	}
	if health.Recoverable {
		t.Error("Recoverable should be false for config errors")
	}

	// Transient error should NOT set ActionRequired
	health2 := DeriveListenerHealth(
		[]string{"wildcard"},
		map[string]*CertificateInfo{
			"wildcard": {
				Status:       "error",
				FailureClass: FailureClassTransient,
				RetryAt:      &retryAt,
			},
		},
		true,
		nil,
	)

	if health2.ActionRequired {
		t.Error("ActionRequired should be false for transient errors")
	}
	if !health2.Recoverable {
		t.Error("Recoverable should be true for transient errors")
	}
}

func TestDeriveListenerHealth_WorstOf(t *testing.T) {
	expiresOK := time.Now().Add(30 * 24 * time.Hour)
	retryAt := time.Now().Add(1 * time.Hour)

	// Multiple certs: one OK, one error → should report error
	health := DeriveListenerHealth(
		[]string{"wildcard", "alias:custom.com"},
		map[string]*CertificateInfo{
			"wildcard":         {Status: "ok", ExpiresAt: &expiresOK},
			"alias:custom.com": {Status: "error", FailureClass: FailureClassConfigError, FailureCode: "cert_dns_error", RetryAt: &retryAt},
		},
		true,
		nil,
	)

	if health.Status != ListenerHealthError {
		t.Errorf("Status = %v, want %v (worst-of)", health.Status, ListenerHealthError)
	}

	// Config error should be preferred for reason when equal severity
	if health.ReasonCode != "cert_dns_error" {
		t.Errorf("ReasonCode = %v, want cert_dns_error (config errors take precedence)", health.ReasonCode)
	}
}

func TestResolveCertificatesForListener(t *testing.T) {
	tests := []struct {
		name          string
		ep            ServiceEndpoint
		remoteEnabled bool
		solver        string
		portalHost    string
		aliases       []RemoteAlias
		wantCerts     []string
		wantRequired  bool
	}{
		{
			name:          "remote disabled",
			ep:            ServiceEndpoint{Name: "web", DerivedHostLabel: "myapp"},
			remoteEnabled: false,
			solver:        "dns-01",
			portalHost:    "example.com",
			wantCerts:     nil,
			wantRequired:  false,
		},
		{
			name:          "dns-01 solver",
			ep:            ServiceEndpoint{Name: "web", DerivedHostLabel: "myapp"},
			remoteEnabled: true,
			solver:        "dns-01",
			portalHost:    "example.com",
			wantCerts:     []string{"wildcard"},
			wantRequired:  true,
		},
		{
			name:          "http-01 solver",
			ep:            ServiceEndpoint{Name: "web", DerivedHostLabel: "myapp"},
			remoteEnabled: true,
			solver:        "http-01",
			portalHost:    "example.com",
			wantCerts:     []string{"host:myapp.example.com"},
			wantRequired:  true,
		},
		{
			name:          "raw listener (no derived host)",
			ep:            ServiceEndpoint{Name: "tcp", DerivedHostLabel: ""},
			remoteEnabled: true,
			solver:        "dns-01",
			portalHost:    "example.com",
			wantCerts:     nil,
			wantRequired:  false,
		},
		{
			name:          "with aliases",
			ep:            ServiceEndpoint{Name: "web", DerivedHostLabel: "myapp"},
			remoteEnabled: true,
			solver:        "dns-01",
			portalHost:    "example.com",
			aliases: []RemoteAlias{
				{Hostname: "custom.com", Listener: "web"},
				{Hostname: "other.com", Listener: "api"},
			},
			wantCerts:    []string{"wildcard", "alias:custom.com"},
			wantRequired: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotCerts, gotRequired := ResolveCertificatesForListener(tt.ep, tt.remoteEnabled, tt.solver, tt.portalHost, tt.aliases)

			if gotRequired != tt.wantRequired {
				t.Errorf("requiresRemoteCert = %v, want %v", gotRequired, tt.wantRequired)
			}

			if len(gotCerts) != len(tt.wantCerts) {
				t.Errorf("certIDs = %v, want %v", gotCerts, tt.wantCerts)
				return
			}

			for i, want := range tt.wantCerts {
				if gotCerts[i] != want {
					t.Errorf("certIDs[%d] = %v, want %v", i, gotCerts[i], want)
				}
			}
		})
	}
}

func TestBackendHealthState_Debouncing(t *testing.T) {
	state := NewBackendHealthState()
	key := "myapp/web"

	// Initial state should be healthy (optimistic)
	isHealthy, _ := state.GetHealthState(key)
	if !isHealthy {
		t.Error("New endpoint should start as healthy")
	}

	// First failure should NOT change state (need 3 consecutive failures)
	result := state.RecordCheck(key, false)
	if !result.IsHealthy {
		t.Error("Should still be healthy after 1 failure")
	}
	if result.StateChanged {
		t.Error("State should not change after 1 failure")
	}

	// Second failure
	result = state.RecordCheck(key, false)
	if !result.IsHealthy {
		t.Error("Should still be healthy after 2 failures")
	}
	if result.StateChanged {
		t.Error("State should not change after 2 failures")
	}

	// Third failure - should now be unhealthy
	result = state.RecordCheck(key, false)
	if result.IsHealthy {
		t.Error("Should be unhealthy after 3 failures")
	}
	if !result.StateChanged {
		t.Error("State should change after 3 failures")
	}

	// Fourth failure - should stay unhealthy, no state change
	result = state.RecordCheck(key, false)
	if result.IsHealthy {
		t.Error("Should remain unhealthy")
	}
	if result.StateChanged {
		t.Error("State should not change (already unhealthy)")
	}

	// Success should immediately recover (threshold = 1)
	result = state.RecordCheck(key, true)
	if !result.IsHealthy {
		t.Error("Should be healthy after 1 success")
	}
	if !result.StateChanged {
		t.Error("State should change (recovered)")
	}
	if result.LastOK == nil {
		t.Error("LastOK should be set after success")
	}
}

func TestBackendHealthState_RemoveEndpoint(t *testing.T) {
	state := NewBackendHealthState()
	key := "myapp/web"

	// Record some state
	state.RecordCheck(key, true)
	state.RecordCheck(key, false)

	// Verify state exists
	_, lastOK := state.GetHealthState(key)
	if lastOK == nil {
		t.Error("LastOK should be set before removal")
	}

	// Remove endpoint
	state.RemoveEndpoint(key)

	// Verify state is cleared (should return optimistic defaults)
	isHealthy, lastOK := state.GetHealthState(key)
	if !isHealthy {
		t.Error("Removed endpoint should return optimistic healthy")
	}
	if lastOK != nil {
		t.Error("Removed endpoint should return nil lastOK")
	}
}

func TestRemoteServiceHostname(t *testing.T) {
	tests := []struct {
		label      string
		portal     string
		want       string
	}{
		{"myapp", "example.com", "myapp.example.com"},
		{"myapp", "EXAMPLE.COM", "myapp.example.com"},
		{"MyApp", "example.com.", "myapp.example.com"},
		{"", "example.com", ""},
		{"myapp", "", ""},
		{"myapp", "  example.com  ", "myapp.example.com"},
	}

	for _, tt := range tests {
		t.Run(tt.label+"_"+tt.portal, func(t *testing.T) {
			got := RemoteServiceHostname(tt.label, tt.portal)
			if got != tt.want {
				t.Errorf("RemoteServiceHostname(%q, %q) = %q, want %q", tt.label, tt.portal, got, tt.want)
			}
		})
	}
}

