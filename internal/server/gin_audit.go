package server

import (
	"time"

	"github.com/gin-gonic/gin"

	"piccolod/internal/events"
	"piccolod/internal/services"
)

// publishAuditEvent publishes an audit event with the given kind and optional metadata.
// Timestamps are always UTC for consistent audit trails.
func (s *GinServer) publishAuditEvent(c *gin.Context, kind string, metadata map[string]any) {
	if s.events == nil {
		return
	}
	s.events.Publish(events.Event{
		Topic: events.TopicAudit,
		Payload: events.AuditEvent{
			Kind:     kind,
			Time:     time.Now().UTC(),
			Source:   c.ClientIP(),
			Metadata: metadata,
		},
	})
}

// publishBackgroundAuditEvent publishes an audit event from a background
// goroutine that has no *gin.Context (and thus no client IP). Used by the
// autounlock orchestrator + scheduler PublishAudit closures. Same shape as
// publishAuditEvent minus Source.
func (s *GinServer) publishBackgroundAuditEvent(kind string, metadata map[string]any) {
	if s.events == nil {
		return
	}
	s.events.Publish(events.Event{
		Topic: events.TopicAudit,
		Payload: events.AuditEvent{
			Kind:     kind,
			Time:     time.Now().UTC(),
			Metadata: metadata,
		},
	})
}

func (s *GinServer) publishTunnelAuthDecision(decision services.TunnelAuthDecision) {
	kind := "tunnel.mtls.allow"
	if !decision.Allowed {
		kind = "tunnel.mtls.deny"
	}
	metadata := map[string]any{
		"app":           decision.App,
		"listener":      decision.Listener,
		"host":          decision.Host,
		"remote_port":   decision.RemotePort,
		"client_ip":     decision.ClientIP,
		"verifier_type": decision.VerifierType,
	}
	if decision.UserID != "" {
		metadata["user_id"] = decision.UserID
	}
	if decision.Username != "" {
		metadata["username"] = decision.Username
	}
	if decision.Role != "" {
		metadata["role"] = decision.Role
	}
	if decision.Serial != "" {
		metadata["serial"] = decision.Serial
	}
	if decision.DenyReason != "" {
		metadata["deny_reason"] = decision.DenyReason
	}
	s.publishBackgroundAuditEvent(kind, metadata)
}
