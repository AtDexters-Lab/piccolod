package server

import "piccolod/internal/events"

// SetTaskRecoveryGlobalSuppression retains the controller-wide recovery delay
// independently of per-app enumeration. This keeps reconnecting clients from
// hydrating as Healthy while the durable recovery marker is intentionally in
// backoff.
func (s *GinServer) SetTaskRecoveryGlobalSuppression(suppressed bool) {
	if s == nil {
		return
	}

	s.taskRecoveryPressureMu.Lock()
	wasSuppressed := s.taskRecoveryGlobalPressure != nil
	var payload events.ResourcePressureEvent
	if suppressed {
		payload = events.ResourcePressureEvent{
			Resource:   events.PressureResourceRuntime,
			Severity:   events.PressureSeverityWarn,
			ReasonCode: "automatic_recovery_suppressed",
			Message:    "Piccolo is recovering. It will retry automatically.",
		}
		s.taskRecoveryGlobalPressure = &payload
	} else {
		s.taskRecoveryGlobalPressure = nil
		payload = events.ResourcePressureEvent{
			Resource:   events.PressureResourceRuntime,
			Severity:   events.PressureSeverityOK,
			ReasonCode: "normal",
			Message:    "Automatic recovery delay cleared",
		}
	}
	s.taskRecoveryPressureMu.Unlock()

	if suppressed == wasSuppressed || s.events == nil {
		return
	}
	s.events.Publish(events.Event{Topic: events.TopicResourcePressure, Payload: payload})
}

func (s *GinServer) taskRecoveryGlobalPressureSnapshot() *events.ResourcePressureEvent {
	if s == nil {
		return nil
	}
	s.taskRecoveryPressureMu.RLock()
	defer s.taskRecoveryPressureMu.RUnlock()
	if s.taskRecoveryGlobalPressure == nil {
		return nil
	}
	payload := *s.taskRecoveryGlobalPressure
	return &payload
}
