package server

import (
	"testing"
	"time"

	"piccolod/internal/events"
	"piccolod/internal/services"
)

func TestPublishTunnelAuthDecisionAudit(t *testing.T) {
	bus := events.NewBus()
	ch := bus.Subscribe(events.TopicAudit, 1)
	srv := &GinServer{events: bus}

	srv.publishTunnelAuthDecision(services.TunnelAuthDecision{
		Allowed:      false,
		Host:         "ssh-demo.example.com",
		RemotePort:   443,
		App:          "demo",
		Listener:     "ssh",
		ClientIP:     "203.0.113.10",
		VerifierType: "piccolo_session",
		UserID:       "user-1",
		Username:     "admin",
		Role:         "admin",
		Serial:       "abc123",
		DenyReason:   services.TunnelAuthReasonSourceIPDenied,
	})

	select {
	case ev := <-ch:
		audit, ok := ev.Payload.(events.AuditEvent)
		if !ok {
			t.Fatalf("audit payload type: got %T", ev.Payload)
		}
		if audit.Kind != "tunnel.mtls.deny" {
			t.Fatalf("audit kind = %q, want tunnel.mtls.deny", audit.Kind)
		}
		for key, want := range map[string]any{
			"app":           "demo",
			"listener":      "ssh",
			"host":          "ssh-demo.example.com",
			"remote_port":   443,
			"client_ip":     "203.0.113.10",
			"verifier_type": "piccolo_session",
			"user_id":       "user-1",
			"username":      "admin",
			"role":          "admin",
			"serial":        "abc123",
			"deny_reason":   services.TunnelAuthReasonSourceIPDenied,
		} {
			if got := audit.Metadata[key]; got != want {
				t.Fatalf("metadata[%s] = %v, want %v", key, got, want)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for tunnel auth audit event")
	}
}
