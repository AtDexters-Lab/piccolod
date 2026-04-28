package services

import (
	"net"
	"strings"
	"sync"
	"time"

	"piccolod/internal/api"
	"piccolod/internal/hostname"
)

// ListenerHealthStatus represents the overall health state of a listener.
type ListenerHealthStatus string

const (
	// ListenerHealthOK: Fully operational, cert valid, backend reachable
	ListenerHealthOK ListenerHealthStatus = "ok"

	// ListenerHealthDegraded: Working but with warnings (still usable)
	// Examples: cert expiring soon (<7 days), high backend latency
	ListenerHealthDegraded ListenerHealthStatus = "degraded"

	// ListenerHealthRecovering: Not working, automatic fix in progress
	// Examples: cert issuance pending, retry scheduled
	ListenerHealthRecovering ListenerHealthStatus = "recovering"

	// ListenerHealthError: Not working (unusable), may or may not require user action
	// Examples: backend unreachable, rate limited (long wait), DNS misconfiguration
	ListenerHealthError ListenerHealthStatus = "error"
)

// ListenerHealth represents the comprehensive health status of an app listener.
type ListenerHealth struct {
	Status         ListenerHealthStatus       `json:"status"`
	ReasonCode     string                     `json:"reason_code"`               // Machine-readable code (stable for UI mapping, dedup, i18n)
	Reason         string                     `json:"reason"`                    // Human-readable explanation (always present)
	Details        *string                    `json:"details,omitempty"`         // Technical details (nil when not applicable)
	RecoveryETA    *time.Time                 `json:"recovery_eta,omitempty"`    // When next retry/check will occur (nil if not scheduled)
	Recoverable    bool                       `json:"recoverable"`               // Can system self-heal without user action?
	ActionRequired bool                       `json:"action_required"`           // Does user need to do something?
	CertStatuses   map[string]CertHealthStatus `json:"cert_statuses,omitempty"` // Per-cert health (certID → status). Empty if no certs needed.
	LastChecked    time.Time                  `json:"last_checked"`
	LastOK         *time.Time                 `json:"last_ok,omitempty"` // When backend was last healthy (nil if never seen healthy)
}

// CertHealthStatus tracks individual certificate health for multi-cert listeners.
// The overall ListenerHealth.Status is the worst-of all CertStatuses + backend health.
type CertHealthStatus struct {
	Status      ListenerHealthStatus `json:"status"`       // ok, recovering, error
	ReasonCode  string               `json:"reason_code"`  // e.g., "cert_pending", "cert_dns_error"
	RecoveryETA *time.Time           `json:"recovery_eta,omitempty"`
}

// CertificateInfo is a minimal interface for certificate data needed for health derivation.
// This avoids an import cycle between services and remote packages.
type CertificateInfo struct {
	Status        string
	FailureClass  string // "transient", "rate_limited", "config_error"
	FailureCode   string
	FailureReason string
	RetryAt       *time.Time
	ExpiresAt     *time.Time
}

// FailureClass constants (mirrored from remote package to avoid import cycle)
const (
	FailureClassTransient   = "transient"
	FailureClassRateLimited = "rate_limited"
	FailureClassConfigError = "config_error"
)

// CertExpirationWarningThreshold is the duration before cert expiry when we report "degraded" status.
const CertExpirationWarningThreshold = 7 * 24 * time.Hour

// strPtr creates a pointer to a string, returning nil for empty strings.
func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// timePtr creates a pointer to a time.Time, returning nil for zero times.
func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// statusWorseThan returns true if a is worse than b.
// Priority: error > recovering > degraded > ok
func statusWorseThan(a, b ListenerHealthStatus) bool {
	priority := map[ListenerHealthStatus]int{
		ListenerHealthOK:         0,
		ListenerHealthDegraded:   1,
		ListenerHealthRecovering: 2,
		ListenerHealthError:      3,
	}
	return priority[a] > priority[b]
}

// DeriveListenerHealth computes health from multiple signals.
// Parameters:
//   - certIDs: certificate IDs this listener depends on (from ResolveCertificatesForListener)
//   - certs: map of certID → CertificateInfo for lookup
//   - backendOK: whether backend TCP check passed
//   - lastOK: when backend was last healthy (from BackendHealthState.lastOK), nil if never seen healthy
//
// Returns aggregated health with per-cert breakdown in CertStatuses.
func DeriveListenerHealth(certIDs []string, certs map[string]*CertificateInfo, backendOK bool, lastOK *time.Time) ListenerHealth {
	certStatuses := make(map[string]CertHealthStatus)

	// Track worst status for aggregation (error > recovering > degraded > ok)
	worstStatus := ListenerHealthOK
	worstReasonCode := "ok"
	worstReason := "Operational"
	var worstDetails *string
	var worstRecoveryETA *time.Time
	recoverable := true
	actionRequired := false

	// 1. Check each certificate
	for _, certID := range certIDs {
		cert := certs[certID]

		var cs CertHealthStatus
		if cert == nil {
			// Certificate not yet issued
			cs = CertHealthStatus{
				Status:     ListenerHealthRecovering,
				ReasonCode: "cert_pending",
			}
		} else {
			switch strings.ToLower(cert.Status) {
			case "error":
				switch cert.FailureClass {
				case FailureClassRateLimited:
					cs = CertHealthStatus{
						Status:      ListenerHealthError,
						ReasonCode:  "cert_rate_limited",
						RecoveryETA: cert.RetryAt,
					}
				case FailureClassConfigError:
					cs = CertHealthStatus{
						Status:      ListenerHealthError,
						ReasonCode:  cert.FailureCode,
						RecoveryETA: cert.RetryAt,
					}
				default: // Transient
					cs = CertHealthStatus{
						Status:      ListenerHealthRecovering,
						ReasonCode:  "cert_retry_scheduled",
						RecoveryETA: cert.RetryAt,
					}
				}
			case "pending":
				cs = CertHealthStatus{
					Status:     ListenerHealthRecovering,
					ReasonCode: "cert_pending",
				}
			case "ok":
				if cert.ExpiresAt != nil && time.Until(*cert.ExpiresAt) < CertExpirationWarningThreshold {
					cs = CertHealthStatus{
						Status:     ListenerHealthDegraded,
						ReasonCode: "cert_expiring_soon",
					}
				} else {
					cs = CertHealthStatus{
						Status:     ListenerHealthOK,
						ReasonCode: "ok",
					}
				}
			default:
				// Unknown status - treat as pending
				cs = CertHealthStatus{
					Status:     ListenerHealthRecovering,
					ReasonCode: "cert_pending",
				}
			}
		}
		certStatuses[certID] = cs

		// Update worst-of aggregation with tie-breaker for actionRequired
		// When status is equal, prefer config errors (actionRequired) over rate limits
		isConfigError := cert != nil && cert.FailureClass == FailureClassConfigError
		shouldUpdate := statusWorseThan(cs.Status, worstStatus) ||
			(cs.Status == worstStatus && isConfigError && !actionRequired)

		if shouldUpdate {
			worstStatus = cs.Status
			worstReasonCode = cs.ReasonCode
			worstReason = reasonForCode(cs.ReasonCode)
			worstRecoveryETA = cs.RecoveryETA
			if cert != nil && cert.FailureReason != "" {
				worstDetails = strPtr(cert.FailureReason)
			}
			// Config errors require user action
			if isConfigError {
				recoverable = false
				actionRequired = true
			}
		}
	}

	// 2. Check backend connectivity
	if !backendOK {
		backendStatus := ListenerHealthError
		if statusWorseThan(backendStatus, worstStatus) {
			worstStatus = backendStatus
			worstReasonCode = "backend_unreachable"
			worstReason = "Backend not responding"
			worstDetails = nil
			worstRecoveryETA = nil
			// Backend issues are auto-recoverable
			recoverable = true
			actionRequired = false
		}
	}

	// 3. Build final health (nil CertStatuses if no certs needed)
	var finalCertStatuses map[string]CertHealthStatus
	if len(certStatuses) > 0 {
		finalCertStatuses = certStatuses
	}

	return ListenerHealth{
		Status:         worstStatus,
		ReasonCode:     worstReasonCode,
		Reason:         worstReason,
		Details:        worstDetails,
		RecoveryETA:    worstRecoveryETA,
		Recoverable:    recoverable,
		ActionRequired: actionRequired,
		CertStatuses:   finalCertStatuses,
		LastChecked:    time.Now(),
		LastOK:         lastOK,
	}
}

// reasonForCode returns human-readable reason for any health/failure code.
// Duplicated from remote/failure.go to avoid import cycle.
func reasonForCode(code string) string {
	reasons := map[string]string{
		// Config error states (require user action)
		"cert_dns_error":               "Domain DNS not configured",
		"cert_domain_unreachable":      "Domain doesn't resolve to this device",
		"cert_caa_forbidden":           "CAA record blocks certificate issuance",
		"cert_rejected_identifier":     "Domain is blocked by certificate authority",
		"cert_invalid_contact":         "Invalid account contact email",
		"cert_account_error":           "Certificate account needs re-registration",
		"cert_unauthorized_persistent": "Challenge verification persistently failing",

		// Transient error states (auto-recoverable)
		"cert_connection_failed": "Unable to reach this device from internet",
		"cert_rate_limited":      "Rate limited by Let's Encrypt",
		"cert_unauthorized":      "Challenge verification failed",
		"cert_acme_error":        "Certificate authority error",
		"cert_unknown_error":     "Unknown error",

		// Recovering/pending states
		"cert_pending":         "Certificate issuance in progress",
		"cert_retry_scheduled": "Certificate issuance failed, retrying",

		// Degraded states
		"cert_expiring_soon": "Certificate expiring soon",

		// OK state
		"ok": "Operational",

		// Backend states
		"backend_unreachable": "Backend not responding",

		// App states (for stopped/starting/error apps)
		"app_starting": "App is starting up",
		"app_error":    "App failed to start",
	}
	if r, ok := reasons[code]; ok {
		return r
	}
	return "Unknown status"
}

// RemoteServiceHostname derives the remote hostname for a listener.
// This follows RFC 20260114: remote hostnames are <label>.<portal> (dot separator).
// Exported for use by both health computation and endpoint formatting.
func RemoteServiceHostname(derivedHostLabel, portalHostname string) string {
	if derivedHostLabel == "" {
		return ""
	}
	base := hostname.Normalize(portalHostname)
	if h, _, err := net.SplitHostPort(base); err == nil {
		base = h
	}
	if base == "" {
		return ""
	}
	return strings.ToLower(derivedHostLabel) + "." + base
}

// ResolveCertificatesForListener determines which certificates a listener's health depends on.
// Returns (certIDs, requiresRemoteCert). If requiresRemoteCert is false, the listener
// doesn't need certificates (local-only or remote disabled).
//
// A listener may have MULTIPLE certificates:
// - Default remote hostname cert (wildcard or per-host depending on solver)
// - One or more alias certs (custom domains pointed to this listener)
func ResolveCertificatesForListener(ep ServiceEndpoint, remoteEnabled bool, solver, portalHostname string, aliases []RemoteAlias) ([]string, bool) {
	// flow:tls listeners manage their own certificates — skip Piccolo cert resolution (RFC 20260316).
	if ep.Flow == api.FlowTLS {
		return nil, false
	}

	// 1. Remote access disabled → no certificates needed
	if !remoteEnabled {
		return nil, false
	}

	var certIDs []string

	// 2. Resolve default remote hostname cert (if listener has derived host label)
	if ep.DerivedHostLabel != "" {
		switch strings.ToLower(solver) {
		case "dns-01":
			// DNS-01 uses wildcard cert for all listeners
			certIDs = append(certIDs, "wildcard")

		case "http-01":
			// HTTP-01 issues per-listener certs based on derived remote hostname
			derivedHost := RemoteServiceHostname(ep.DerivedHostLabel, portalHostname)
			if derivedHost != "" {
				certIDs = append(certIDs, "host:"+derivedHost)
			}
		}
	}

	// 3. Add any alias certs for this listener.
	// Alias.Listener stores a DerivedHostLabel or "__portal". Match against ep.DerivedHostLabel.
	for _, alias := range aliases {
		if alias.Listener == portalHostLabel || alias.Listener == "" {
			continue // portal aliases use the portal cert, not per-listener
		}
		if alias.Listener == ep.DerivedHostLabel {
			certIDs = append(certIDs, "alias:"+strings.ToLower(alias.Hostname))
		}
	}

	// 4. If no certs found (raw/tls listener with no aliases), no cert needed
	if len(certIDs) == 0 {
		return nil, false
	}

	return certIDs, true
}

// RemoteAlias is a simplified alias type for health derivation.
// Listener stores a DerivedHostLabel (e.g., "myapp", "home-myapp") or "__portal".
type RemoteAlias struct {
	Hostname string
	Listener string
}

// RemoteCertificate represents a certificate from the remote manager for health computation.
type RemoteCertificate struct {
	ID            string
	Domains       []string
	Status        string
	FailureClass  string
	FailureCode   string
	FailureReason string
	RetryAt       *time.Time
	ExpiresAt     *time.Time
}

// RemoteStatus represents remote access status needed for health computation.
type RemoteStatus struct {
	Enabled        bool
	Solver         string
	PortalHostname string
}

// RemoteStatusProvider abstracts the remote manager for health computation.
// Implementations should be thread-safe.
type RemoteStatusProvider interface {
	// GetRemoteStatus returns the current remote access configuration.
	GetRemoteStatus() RemoteStatus
	// GetCertificates returns all certificates managed by the remote manager.
	GetCertificates() []RemoteCertificate
	// GetAliases returns all remote aliases.
	GetAliases() []RemoteAlias
}

// resolveCertID maps a remote certificate to its canonical ID for health lookup.
// For wildcard certs: "wildcard"
// For per-host certs: "host:<hostname>"
// For alias certs: "alias:<hostname>"
func resolveCertID(id string, domains []string) string {
	// Check if this is a wildcard cert
	if id == "wildcard" {
		return "wildcard"
	}

	// Check if this is an alias cert
	if strings.HasPrefix(id, "alias:") {
		return id
	}

	// Per-host certs use first domain
	if len(domains) > 0 {
		return "host:" + strings.ToLower(domains[0])
	}

	return id
}

// Backend health debouncing constants (RFC 20260125)
const (
	// backendFailureThreshold is consecutive failures before reporting unhealthy.
	backendFailureThreshold = 3

	// backendRecoveryThreshold is consecutive successes before reporting healthy.
	backendRecoveryThreshold = 1
)

// BackendHealthState tracks debounced backend health per endpoint.
// Prevents flapping by requiring multiple consecutive failures before marking unhealthy.
type BackendHealthState struct {
	mu             sync.RWMutex
	lastOK         map[string]time.Time // endpoint key → last successful check time
	failureCount   map[string]int       // endpoint key → consecutive failure count
	successCount   map[string]int       // endpoint key → consecutive success count
	lastEmittedOK  map[string]bool      // endpoint key → last emitted health state (true=healthy)
	firstSeen      map[string]time.Time // endpoint key → first time this endpoint was seen
}

// NewBackendHealthState creates a new backend health state tracker.
func NewBackendHealthState() *BackendHealthState {
	return &BackendHealthState{
		lastOK:        make(map[string]time.Time),
		failureCount:  make(map[string]int),
		successCount:  make(map[string]int),
		lastEmittedOK: make(map[string]bool),
		firstSeen:     make(map[string]time.Time),
	}
}

// RecordCheckResult holds the result of recording a backend health check.
type RecordCheckResult struct {
	IsHealthy    bool       // Current debounced health state
	StateChanged bool       // Whether health state changed (for event emission)
	LastOK       *time.Time // When backend was last healthy (nil if never seen healthy)
}

// RecordCheck records a backend check result and returns the debounced health state.
// Uses hysteresis: requires backendFailureThreshold consecutive failures to mark unhealthy,
// and backendRecoveryThreshold consecutive successes to mark healthy again.
func (s *BackendHealthState) RecordCheck(endpointKey string, checkOK bool) RecordCheckResult {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Track first seen time for new endpoints
	if _, exists := s.firstSeen[endpointKey]; !exists {
		s.firstSeen[endpointKey] = time.Now()
		// New endpoints start as healthy (optimistic)
		s.lastEmittedOK[endpointKey] = true
	}

	prevEmitted := s.lastEmittedOK[endpointKey]

	if checkOK {
		// Success: reset failure count, increment success count
		s.failureCount[endpointKey] = 0
		s.successCount[endpointKey]++
		s.lastOK[endpointKey] = time.Now()

		// Recover after enough consecutive successes
		if !prevEmitted && s.successCount[endpointKey] >= backendRecoveryThreshold {
			s.lastEmittedOK[endpointKey] = true
			s.successCount[endpointKey] = 0 // Reset after recovery to avoid unbounded growth
			return RecordCheckResult{
				IsHealthy:    true,
				StateChanged: true,
				LastOK:       timePtr(s.lastOK[endpointKey]),
			}
		}
	} else {
		// Failure: reset success count, increment failure count
		s.successCount[endpointKey] = 0
		s.failureCount[endpointKey]++

		// Mark unhealthy after enough consecutive failures
		if prevEmitted && s.failureCount[endpointKey] >= backendFailureThreshold {
			s.lastEmittedOK[endpointKey] = false
			var lastOKPtr *time.Time
			if t, ok := s.lastOK[endpointKey]; ok {
				lastOKPtr = &t
			}
			return RecordCheckResult{
				IsHealthy:    false,
				StateChanged: true,
				LastOK:       lastOKPtr,
			}
		}
	}

	// No state change
	var lastOKPtr *time.Time
	if t, ok := s.lastOK[endpointKey]; ok {
		lastOKPtr = &t
	}
	return RecordCheckResult{
		IsHealthy:    s.lastEmittedOK[endpointKey],
		StateChanged: false,
		LastOK:       lastOKPtr,
	}
}

// GetHealthState returns the current debounced health state for an endpoint.
// Returns (isHealthy, lastOK). If endpoint is unknown, returns (true, nil) (optimistic).
func (s *BackendHealthState) GetHealthState(endpointKey string) (bool, *time.Time) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	isHealthy, exists := s.lastEmittedOK[endpointKey]
	if !exists {
		return true, nil // Optimistic for unknown endpoints
	}

	var lastOK *time.Time
	if t, ok := s.lastOK[endpointKey]; ok {
		lastOK = &t
	}
	return isHealthy, lastOK
}

// RemoveEndpoint removes an endpoint from tracking (called when listener is removed).
func (s *BackendHealthState) RemoveEndpoint(endpointKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.lastOK, endpointKey)
	delete(s.failureCount, endpointKey)
	delete(s.successCount, endpointKey)
	delete(s.lastEmittedOK, endpointKey)
	delete(s.firstSeen, endpointKey)
}
