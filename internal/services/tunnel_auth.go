package services

import (
	"context"
	"crypto/x509"
	"strings"
	"sync"
	"time"

	"piccolod/internal/api"
)

const (
	TunnelAuthReasonUnsupportedVerifier      = "unsupported_verifier"
	TunnelAuthReasonMissingClientCertificate = "missing_client_certificate"
	TunnelAuthReasonInvalidClientCertificate = "invalid_client_certificate"
	TunnelAuthReasonUnknownCertificateSerial = "unknown_certificate_serial"
	TunnelAuthReasonCertificateExpired       = "certificate_expired"
	TunnelAuthReasonAudienceMismatch         = "audience_mismatch"
	TunnelAuthReasonSourceIPDenied           = "source_ip_denied"
	TunnelAuthReasonVerifierUnavailable      = "verifier_unavailable"
	TunnelAuthReasonMissingRouteMetadata     = "missing_route_metadata"
	TunnelAuthReasonVerificationFailed       = "verification_failed"
)

// TunnelClientVerification is the TLS-mux admission request for listeners that
// declare connection_auth.mtls.verifier.type=piccolo_session.
type TunnelClientVerification struct {
	PeerCertificates []*x509.Certificate
	Host             string
	RemotePort       int
	App              string
	Listener         string
	ClientIP         string
	ConnectionAuth   *api.ConnectionAuth
	Now              time.Time
}

// TunnelClientVerificationResult carries the accepted tunnel identity and
// connection lifetime bound.
type TunnelClientVerificationResult struct {
	UserID   string
	Username string
	Role     string
	Serial   string
	NotAfter time.Time
}

type TunnelClientVerificationError struct {
	Reason   string
	Identity TunnelClientVerificationResult
	Err      error
}

func NewTunnelClientVerificationError(reason string, err error, identity TunnelClientVerificationResult) *TunnelClientVerificationError {
	return &TunnelClientVerificationError{Reason: reason, Err: err, Identity: identity}
}

func (e *TunnelClientVerificationError) Error() string {
	if e == nil {
		return ""
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return e.Reason
}

func (e *TunnelClientVerificationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// TunnelClientVerifier verifies Piccolo-issued client certs for TLS-mux routes.
type TunnelClientVerifier interface {
	VerifyTunnelClient(context.Context, TunnelClientVerification) (TunnelClientVerificationResult, error)
}

type TunnelAuthDecision struct {
	Allowed      bool
	Host         string
	RemotePort   int
	App          string
	Listener     string
	ClientIP     string
	VerifierType string
	UserID       string
	Username     string
	Role         string
	Serial       string
	DenyReason   string
	Time         time.Time
}

type TunnelAuthDecisionRecorder func(TunnelAuthDecision)

type TunnelAuthMetricsSample struct {
	App          string
	Listener     string
	SourceIP     string
	VerifierType string
	DenyReason   string
}

type TunnelAuthMetricsSnapshot struct {
	Allowed map[TunnelAuthMetricsSample]uint64
	Denied  map[TunnelAuthMetricsSample]uint64
}

type TunnelAuthMetricsRegistry struct {
	mu      sync.Mutex
	allowed map[TunnelAuthMetricsSample]uint64
	denied  map[TunnelAuthMetricsSample]uint64
}

func NewTunnelAuthMetricsRegistry() *TunnelAuthMetricsRegistry {
	return &TunnelAuthMetricsRegistry{
		allowed: map[TunnelAuthMetricsSample]uint64{},
		denied:  map[TunnelAuthMetricsSample]uint64{},
	}
}

func (r *TunnelAuthMetricsRegistry) Record(decision TunnelAuthDecision) {
	if r == nil {
		return
	}
	sample := TunnelAuthMetricsSample{
		App:          decision.App,
		Listener:     decision.Listener,
		SourceIP:     sourceIPMetricLabel(decision.ClientIP),
		VerifierType: decision.VerifierType,
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if decision.Allowed {
		r.allowed[sample]++
		return
	}
	sample.DenyReason = decision.DenyReason
	r.denied[sample]++
}

func (r *TunnelAuthMetricsRegistry) Snapshot() TunnelAuthMetricsSnapshot {
	snap := TunnelAuthMetricsSnapshot{
		Allowed: map[TunnelAuthMetricsSample]uint64{},
		Denied:  map[TunnelAuthMetricsSample]uint64{},
	}
	if r == nil {
		return snap
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, v := range r.allowed {
		snap.Allowed[k] = v
	}
	for k, v := range r.denied {
		snap.Denied[k] = v
	}
	return snap
}

func sourceIPMetricLabel(ip string) string {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return "unknown"
	}
	return ip
}
