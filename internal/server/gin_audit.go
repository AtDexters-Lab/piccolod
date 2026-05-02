package server

import (
	"time"

	"github.com/gin-gonic/gin"

	"piccolod/internal/events"
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
