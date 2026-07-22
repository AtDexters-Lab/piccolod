package pressure

import (
	"context"
	"errors"
	"testing"
)

func TestAdmissionGateFenceAndTransitionContinuation(t *testing.T) {
	gate := NewAdmissionGate()
	if err := gate.Check(context.Background(), WorkPodman); err != nil {
		t.Fatalf("normal admission: %v", err)
	}
	gate.Fence()
	err := gate.Check(context.Background(), WorkPodman)
	if !errors.Is(err, ErrTaskPressure) || !IsAdmissionError(err) {
		t.Fatalf("fenced error = %v, want typed task-pressure error", err)
	}
	if err := gate.Check(WithTransitionContinuation(context.Background()), WorkPodman); err != nil {
		t.Fatalf("recorded transition continuation rejected: %v", err)
	}
	gate.OpenPressure()
	if gate.Fenced() {
		t.Fatal("gate remained fenced after Open")
	}
}

func TestAdmissionGateStartupFenceIsIndependent(t *testing.T) {
	gate := NewAdmissionGate()
	gate.FenceStartup()
	if err := gate.Check(context.Background(), WorkStorage); !IsAdmissionError(err) {
		t.Fatalf("startup fence error = %v, want typed task-pressure error", err)
	}
	if err := gate.Check(WithTransitionContinuation(context.Background()), WorkStorage); !IsAdmissionError(err) {
		t.Fatalf("transition bypassed startup fence: %v", err)
	}
	gate.Fence()
	gate.OpenStartup()
	if !gate.Fenced() {
		t.Fatal("opening startup fence also opened pressure fence")
	}
	gate.OpenPressure()
	if gate.Fenced() {
		t.Fatal("gate remained fenced after both reasons opened")
	}
}

func TestAdmissionGateCriticalFenceRejectsTransitionContinuation(t *testing.T) {
	gate := NewAdmissionGate()
	gate.FenceCritical()
	err := gate.Check(WithTransitionContinuation(context.Background()), WorkPodman)
	if !IsAdmissionError(err) {
		t.Fatalf("critical fence error = %v, want typed task-pressure error", err)
	}
	gate.OpenPressure()
	if !gate.Fenced() {
		t.Fatal("normal pressure recovery reopened the process-fatal fence")
	}
	gate.ResetForTest()
}

func TestAdmissionGateMonitorUnavailableAllowsOnlyCoreStartup(t *testing.T) {
	gate := NewAdmissionGate()
	gate.BeginCoreStartup()
	gate.FenceUnavailable()
	if err := gate.Check(context.Background(), WorkStorage); err != nil {
		t.Fatalf("monitor-unavailable fence rejected bounded core startup: %v", err)
	}

	gate.EndCoreStartup()
	if err := gate.Check(context.Background(), WorkStorage); !IsAdmissionError(err) {
		t.Fatalf("monitor-unavailable fence admitted optional work: %v", err)
	}
	if err := gate.Check(WithTransitionContinuation(context.Background()), WorkStorage); !IsAdmissionError(err) {
		t.Fatalf("transition bypassed monitor-unavailable fence: %v", err)
	}

	gate.OpenPressure()
	if gate.Fenced() {
		t.Fatal("gate remained unavailable-fenced after a normal sample")
	}
}
