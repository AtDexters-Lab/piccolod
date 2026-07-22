package server

import (
	"reflect"
	"testing"

	"piccolod/internal/events"
	"piccolod/internal/resources/pressure"
)

func TestResourcePressureTopicMapsBusEvent(t *testing.T) {
	if topics := supportedTopics[topicResourcePressure]; len(topics) != 1 || topics[0] != events.TopicResourcePressure {
		t.Fatalf("resource pressure topics = %v", topics)
	}
	srv := &GinServer{}
	payload := events.ResourcePressureEvent{Resource: events.PressureResourceTasks, Severity: events.PressureSeverityWarn}
	msg := srv.processEvent(topicResourcePressure, events.Event{Topic: events.TopicResourcePressure, Payload: payload}, func(string) bool { return false }, false)
	if msg == nil || msg.Type != topicResourcePressure || !reflect.DeepEqual(msg.Payload, payload) {
		t.Fatalf("unexpected stream message: %#v", msg)
	}
}

func TestResourcePressureTopicFiltersRuntimeApps(t *testing.T) {
	srv := &GinServer{}
	payload := events.ResourcePressureEvent{
		Resource: events.PressureResourceRuntime, AppInstanceID: "private-app",
		Severity: events.PressureSeverityWarn,
	}
	if msg := srv.processEvent(topicResourcePressure, events.Event{Topic: events.TopicResourcePressure, Payload: payload}, func(string) bool { return false }, false); msg != nil {
		t.Fatalf("unauthorized runtime pressure leaked: %#v", msg)
	}
	if msg := srv.processEvent(topicResourcePressure, events.Event{Topic: events.TopicResourcePressure, Payload: payload}, func(id string) bool { return id == "private-app" }, false); msg == nil {
		t.Fatal("authorized runtime pressure was filtered")
	}
}

func TestInitialTaskPressureSnapshotIncludesRecoveryState(t *testing.T) {
	guard := pressure.NewTaskGuard(pressure.TaskGuardConfig{})
	guardSnapshot := guard.Snapshot()
	if guardSnapshot.State != pressure.TaskPressureUnavailable {
		t.Fatalf("guard state=%s", guardSnapshot.State)
	}
	srv := &GinServer{taskGuard: guard}
	var got streamMessage
	srv.sendInitialResourcePressure(func(string) bool { return true }, func(v any) error {
		got = v.(streamMessage)
		return nil
	})
	payload, ok := got.Payload.(events.ResourcePressureEvent)
	if !ok || got.Type != topicResourcePressure {
		t.Fatalf("unexpected initial message: %#v", got)
	}
	if payload.Resource != events.PressureResourceTasks || payload.Severity != events.PressureSeverityWarn || payload.ReasonCode != pressure.ReasonMonitorUnavailable {
		t.Fatalf("unexpected initial payload: %+v", payload)
	}
}

func TestInitialResourcePressureSnapshotIncludesGlobalRecoveryBackoff(t *testing.T) {
	srv := &GinServer{}
	srv.SetTaskRecoveryGlobalSuppression(true)
	var got []streamMessage
	srv.sendInitialResourcePressure(func(string) bool { return true }, func(v any) error {
		got = append(got, v.(streamMessage))
		return nil
	})
	if len(got) != 2 {
		t.Fatalf("initial pressure messages = %d, want task + recovery", len(got))
	}
	payload, ok := got[1].Payload.(events.ResourcePressureEvent)
	if !ok || payload.ReasonCode != "automatic_recovery_suppressed" || payload.AppInstanceID != "" {
		t.Fatalf("global recovery payload = %#v", got[1].Payload)
	}

	srv.SetTaskRecoveryGlobalSuppression(false)
	if snapshot := srv.taskRecoveryGlobalPressureSnapshot(); snapshot != nil {
		t.Fatalf("global recovery snapshot retained after clear: %+v", snapshot)
	}
}
