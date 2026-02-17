package remote

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	legoacme "github.com/go-acme/lego/v4/acme"
)

// FailureClass categorizes certificate issuance failures for retry behavior.
type FailureClass string

const (
	// FailureClassTransient indicates a temporary issue, retry with normal backoff.
	// Examples: network timeout, DNS propagation delay, ACME server hiccup.
	FailureClassTransient FailureClass = "transient"

	// FailureClassRateLimited indicates Let's Encrypt rate limit was hit.
	// Requires long backoff (hours/days).
	FailureClassRateLimited FailureClass = "rate_limited"

	// FailureClassConfigError indicates user configuration issue requiring action.
	// Examples: invalid domain, DNS not pointing to Piccolo, CAA record blocks issuance.
	// Auto-retries weekly to catch external fixes.
	FailureClassConfigError FailureClass = "config_error"
)

// Escalation thresholds for persistent transient errors
const (
	// ConnectionEscalateAfterAttempts is the number of connection failures before escalating to config error.
	ConnectionEscalateAfterAttempts = 5
	// ConnectionEscalateAfterDuration is the time window for connection failures before escalating.
	ConnectionEscalateAfterDuration = 24 * time.Hour

	// UnauthorizedEscalateAfterAttempts is the number of unauthorized errors before escalating.
	UnauthorizedEscalateAfterAttempts = 5
	// UnauthorizedEscalateAfterDuration is the time window for unauthorized errors before escalating.
	UnauthorizedEscalateAfterDuration = 24 * time.Hour

	// ConfigErrorProbeInterval is the weekly probe interval for config errors.
	ConfigErrorProbeInterval = 7 * 24 * time.Hour
)

// classifyFailure determines the failure class and code from an ACME error.
// Returns (FailureClass, FailureCode) where FailureCode uses canonical "cert_*" namespace.
func classifyFailure(err error) (FailureClass, string) {
	if err == nil {
		return FailureClassTransient, "cert_unknown_error"
	}

	// Check for rate limit error first (has special type)
	if isRateLimitError(err) {
		return FailureClassRateLimited, "cert_rate_limited"
	}

	// Check for ACME ProblemDetails
	var pd *legoacme.ProblemDetails
	if !errors.As(err, &pd) {
		// Not an ACME ProblemDetails — check for network-level errors
		// that should use the connection-failure escalation path.
		if isNetworkError(err) {
			return FailureClassTransient, "cert_connection_failed"
		}
		return FailureClassTransient, "cert_unknown_error"
	}

	switch pd.Type {
	case "urn:ietf:params:acme:error:rateLimited":
		return FailureClassRateLimited, "cert_rate_limited"

	case "urn:ietf:params:acme:error:dns":
		// DNS resolution failed - likely user config error
		return FailureClassConfigError, "cert_dns_error"

	case "urn:ietf:params:acme:error:unauthorized":
		// Domain validation failed - could be transient or config
		// Certain detail strings indicate definite config errors
		if strings.Contains(pd.Detail, "No valid IP addresses") ||
			strings.Contains(pd.Detail, "NXDOMAIN") ||
			strings.Contains(pd.Detail, "Incorrect TXT record") {
			return FailureClassConfigError, "cert_domain_unreachable"
		}
		// Otherwise starts as transient but may escalate
		return FailureClassTransient, "cert_unauthorized"

	case "urn:ietf:params:acme:error:connection":
		// Connection failed - starts transient, may escalate
		return FailureClassTransient, "cert_connection_failed"

	case "urn:ietf:params:acme:error:caa":
		// CAA record blocks issuance - user must fix DNS
		return FailureClassConfigError, "cert_caa_forbidden"

	case "urn:ietf:params:acme:error:rejectedIdentifier":
		// Domain is on a blocklist or policy prohibits issuance
		return FailureClassConfigError, "cert_rejected_identifier"

	case "urn:ietf:params:acme:error:invalidContact":
		// Invalid contact email - user must fix account
		return FailureClassConfigError, "cert_invalid_contact"

	case "urn:ietf:params:acme:error:accountDoesNotExist":
		// Account was deactivated/deleted - needs re-registration
		return FailureClassConfigError, "cert_account_error"

	default:
		// Unknown error types start as transient
		return FailureClassTransient, "cert_acme_error"
	}
}

// isRateLimitError checks if the error is a rate limit error.
func isRateLimitError(err error) bool {
	if err == nil {
		return false
	}

	// 1. Check lego's RateLimitedError (preferred, includes RetryAfter)
	var rle *legoacme.RateLimitedError
	if errors.As(err, &rle) {
		return true
	}

	// 2. Check ProblemDetails for rate limit type or 429 status
	var pd *legoacme.ProblemDetails
	if errors.As(err, &pd) {
		if pd.Type == "urn:ietf:params:acme:error:rateLimited" {
			return true
		}
		if pd.HTTPStatus == 429 {
			return true
		}
	}

	// 3. Fallback: string matching for edge cases or wrapped errors
	errStr := strings.ToLower(err.Error())
	rateLimitPatterns := []string{
		"rate limit",
		"too many requests",
		"too many certificates",
		"too many failed authorizations",
		"ratelimited",
	}
	for _, pattern := range rateLimitPatterns {
		if strings.Contains(errStr, pattern) {
			return true
		}
	}

	return false
}

// parseRetryAfter extracts Retry-After from lego's RateLimitedError.
// Returns nil if not a rate limit error or if Retry-After is unparseable.
func parseRetryAfter(err error) *time.Time {
	var rle *legoacme.RateLimitedError
	if !errors.As(err, &rle) || rle.RetryAfter == "" {
		return nil
	}

	// Retry-After can be seconds (integer) or HTTP-date
	if secs, parseErr := strconv.Atoi(rle.RetryAfter); parseErr == nil {
		t := time.Now().Add(time.Duration(secs) * time.Second)
		return &t
	}
	if t, parseErr := http.ParseTime(rle.RetryAfter); parseErr == nil {
		return &t
	}
	return nil
}

// isNetworkError checks whether err represents a network-level failure
// (DNS resolution, TCP connect, TLS handshake, HTTP transport) that should
// be classified as cert_connection_failed for proper escalation.
func isNetworkError(err error) bool {
	if err == nil {
		return false
	}
	// Exclude shutdown cancellations — not a network failure.
	if errors.Is(err, context.Canceled) {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true // covers *net.OpError, *net.DNSError, timeouts
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return true // transport-level failures from http.Client
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	// String fallback for wrapped errors without proper types
	msg := err.Error()
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "i/o timeout")
}

// ReasonForCode returns human-readable reason for any health/failure code.
// Covers all canonical codes: error states (cert_*), non-error states, backend codes, and app states.
func ReasonForCode(code string) string {
	reasons := map[string]string{
		// Config error states (require user action)
		"cert_dns_error":               "Domain DNS not configured",
		"cert_domain_unreachable":      "Domain doesn't resolve to this device",
		"cert_caa_forbidden":           "CAA record blocks certificate issuance",
		"cert_rejected_identifier":     "Domain is blocked by certificate authority",
		"cert_invalid_contact":         "Invalid account contact email",
		"cert_account_error":           "Certificate account needs re-registration",
		"cert_unauthorized_persistent": "Challenge verification persistently failing",
		"cert_orchestrator_error":      "Piccolo orchestrator unavailable",

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
