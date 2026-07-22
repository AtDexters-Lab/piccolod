package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"piccolod/internal/resources/pressure"
)

type blockingTaskEmergencyLogWriter struct {
	blocked chan struct{}
}

func (w blockingTaskEmergencyLogWriter) Write(p []byte) (int, error) {
	<-w.blocked
	return len(p), nil
}

func TestTaskEmergencyOwnerBlockedSinkHonorsDeadline(t *testing.T) {
	if os.Getenv("PICCOLO_TASK_EMERGENCY_HELPER") == "1" {
		taskRecoveryMarkerPath = os.Getenv("PICCOLO_TASK_EMERGENCY_MARKER")
		log.SetOutput(blockingTaskEmergencyLogWriter{blocked: make(chan struct{})})
		census := make(chan pressure.TaskCensus, 1)
		if !processTaskFatalRequests.RequestCritical(pressure.TaskSnapshot{
			State:        pressure.TaskPressureCritical,
			ReasonCode:   pressure.ReasonHighWater,
			Current:      80,
			Limit:        100,
			CurrentKnown: true,
			LimitKnown:   true,
		}) {
			os.Exit(98)
		}
		census <- pressure.TaskCensus{Goroutines: 1}
		runTaskEmergencyOwner(census)
		os.Exit(99)
	}

	markerPath := filepath.Join(t.TempDir(), "task-recovery.json")
	cmd := exec.Command(os.Args[0], "-test.run=^TestTaskEmergencyOwnerBlockedSinkHonorsDeadline$")
	cmd.Env = append(os.Environ(),
		"PICCOLO_TASK_EMERGENCY_HELPER=1",
		"PICCOLO_TASK_EMERGENCY_MARKER="+markerPath,
	)
	started := time.Now()
	err := cmd.Run()
	elapsed := time.Since(started)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != taskEmergencyExitCode {
		t.Fatalf("helper err = %v, want exit code %d", err, taskEmergencyExitCode)
	}
	if elapsed > 3*taskEmergencyDeadline {
		t.Fatalf("blocked sink extended emergency exit to %s", elapsed)
	}
}

func TestTaskRecoveryMarkerRoundTripAndGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run", "task-recovery.json")
	snapshot := pressure.TaskSnapshot{
		ReasonCode:   pressure.ReasonHighWater,
		Current:      1800,
		Limit:        2311,
		CurrentKnown: true,
		LimitKnown:   true,
	}
	now := time.Unix(100, 0)
	first := buildTaskRecoveryMarkerForInvocationAt(snapshot, &pressure.TaskCensus{LifecycleOwner: "app:namek"}, taskRecoveryMarker{}, "invocation-1", now)
	if err := writeTaskRecoveryMarker(path, first); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	got, present, err := loadTaskRecoveryMarker(path)
	if err != nil || !present {
		t.Fatalf("load marker: present=%v err=%v", present, err)
	}
	if got.Generation != 1 || len(got.Suspects) != 1 || got.Suspects[0].Owner != "app:namek" ||
		got.Suspects[0].Strike != 1 || got.TaskCurrent == nil || *got.TaskCurrent != 1800 {
		t.Fatalf("unexpected marker: %+v", got)
	}
	second := buildTaskRecoveryMarkerForInvocationAt(snapshot, nil, got, "invocation-2", now.Add(time.Second))
	if second.Generation != 2 || second.GlobalStrike != 1 {
		t.Fatalf("generation=%d, want 2", second.Generation)
	}
}

func TestTaskRecoveryMarkerDoesNotCarryStaleTaskCountsIntoLaterFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task-recovery.json")
	current, limit := int64(2200), int64(2311)
	previous := taskRecoveryMarker{
		SchemaVersion:          taskRecoveryMarkerSchema,
		Timestamp:              time.Unix(100, 0).UTC(),
		DetectionAt:            time.Unix(99, 0).UTC(),
		ReasonCode:             pressure.ReasonHighWater,
		TaskCurrent:            &current,
		TaskLimit:              &limit,
		Generation:             1,
		LastFailedInvocationID: "critical-invocation",
	}
	if err := writeTaskRecoveryMarker(path, previous); err != nil {
		t.Fatal(err)
	}
	if err := recordServiceExitWithHandoff(path, "signal", "9", "watchdog-invocation", time.Unix(200, 0).UTC(), false); err != nil {
		t.Fatal(err)
	}
	got, present, err := loadTaskRecoveryMarker(path)
	if err != nil || !present {
		t.Fatalf("load marker: present=%v err=%v", present, err)
	}
	if got.TaskCurrent != nil || got.TaskLimit != nil {
		t.Fatalf("later failure retained stale task counts: %+v", got)
	}
	if !got.DetectionAt.Equal(time.Unix(200, 0).UTC()) {
		t.Fatalf("later failure detection=%s, want service-exit observation", got.DetectionAt)
	}
}

func TestTaskRecoveryMarkerHasInitialBackoff(t *testing.T) {
	tests := []struct {
		name   string
		marker taskRecoveryMarker
		want   bool
	}{
		{name: "first ordinary strike is immediate", marker: taskRecoveryMarker{Suspects: []taskRecoverySuspect{{Owner: "app:alpha", Strike: 1}}}},
		{name: "recurrent app is delayed", marker: taskRecoveryMarker{Suspects: []taskRecoverySuspect{{Owner: "app:alpha", Strike: 2}}}, want: true},
		{name: "unlock retry is delayed", marker: taskRecoveryMarker{Suspects: []taskRecoverySuspect{{Owner: taskRecoveryUnlockChainOwner, Strike: 1}}}, want: true},
		{name: "global recurrence is delayed", marker: taskRecoveryMarker{GlobalStrike: 2}, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := taskRecoveryMarkerHasInitialBackoff(test.marker); got != test.want {
				t.Fatalf("taskRecoveryMarkerHasInitialBackoff() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestMalformedTaskRecoveryMarkerStillRequestsRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task-recovery.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, present, err := loadTaskRecoveryMarker(path)
	if !present || err == nil {
		t.Fatalf("present=%v err=%v, want recovery with decode error", present, err)
	}
}

func TestBuildTaskRecoveryMarkerOmitsUnknownCounts(t *testing.T) {
	now := time.Unix(200, 0)
	marker := buildTaskRecoveryMarkerForInvocationAt(
		pressure.TaskSnapshot{ReasonCode: pressure.ReasonMaxEvent},
		nil,
		taskRecoveryMarker{Generation: 4},
		"invocation-5",
		now,
	)
	if marker.Generation != 5 || marker.TaskCurrent != nil || marker.TaskLimit != nil {
		t.Fatalf("unexpected marker: %+v", marker)
	}
	if !marker.Timestamp.Equal(now) {
		t.Fatalf("timestamp=%s, want %s", marker.Timestamp, now)
	}
}

func TestBuildTaskRecoveryMarkerAdvancesRepeatedOwnerStrike(t *testing.T) {
	previous := taskRecoveryMarker{
		SchemaVersion: taskRecoveryMarkerSchema,
		Timestamp:     time.Now().UTC().Add(-time.Minute),
		ReasonCode:    pressure.ReasonHighWater,
		Generation:    1,
		Suspects:      []taskRecoverySuspect{{Owner: "app:namek", Strike: 1}},
	}
	now := time.Unix(300, 0)
	marker := buildTaskRecoveryMarkerForInvocationAt(
		pressure.TaskSnapshot{ReasonCode: pressure.ReasonHighWater},
		&pressure.TaskCensus{LifecycleOwner: "app:namek"},
		previous,
		"invocation-2",
		now,
	)
	if len(marker.Suspects) != 1 || marker.Suspects[0] != (taskRecoverySuspect{Owner: "app:namek", Strike: 2}) {
		t.Fatalf("unexpected repeated suspect: %+v", marker)
	}
	marker = buildTaskRecoveryMarkerForInvocationAt(
		pressure.TaskSnapshot{ReasonCode: pressure.ReasonHighWater},
		&pressure.TaskCensus{LifecycleOwner: "app:other"},
		previous,
		"invocation-3",
		now.Add(time.Second),
	)
	if len(marker.Suspects) != 2 || marker.Suspects[1] != (taskRecoverySuspect{Owner: "app:other", Strike: 1}) {
		t.Fatalf("different owner should receive its own strike: %+v", marker)
	}
}

func TestTaskRecoveryFailureIsInvocationIdempotent(t *testing.T) {
	first := advanceTaskRecoveryFailure(taskRecoveryMarker{}, "storage", "same-invocation", pressure.ReasonHighWater, time.Now())
	second := advanceTaskRecoveryFailure(first, "storage", "same-invocation", pressure.ReasonHighWater, time.Now().Add(time.Second))
	if second.Generation != 1 || len(second.Suspects) != 1 || second.Suspects[0].Strike != 1 {
		t.Fatalf("same invocation advanced twice: %+v", second)
	}
}

func TestTaskRecoverySuspectOverflowAdvancesGlobal(t *testing.T) {
	previous := taskRecoveryMarker{Generation: 8}
	for i := 0; i < taskRecoverySuspectLimit; i++ {
		previous.Suspects = append(previous.Suspects, taskRecoverySuspect{Owner: fmt.Sprintf("owner-%d", i), Strike: 1})
	}
	got := advanceTaskRecoveryFailure(previous, "owner-overflow", "invocation-9", "service_failure", time.Now())
	if len(got.Suspects) != taskRecoverySuspectLimit || got.GlobalStrike != 1 {
		t.Fatalf("overflow marker = %+v", got)
	}
}

func TestRecordServiceExitPreservesEmergencyAdvancement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task-recovery.json")
	marker := advanceTaskRecoveryFailure(taskRecoveryMarker{}, "storage", "invocation-1", pressure.ReasonHighWater, time.Now())
	if err := writeTaskRecoveryMarker(path, marker); err != nil {
		t.Fatal(err)
	}
	if err := recordServiceExit(path, "exit-code", "75", "invocation-1", time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	got, _, _ := loadTaskRecoveryMarker(path)
	if got.Generation != 1 || got.Suspects[0].Strike != 1 {
		t.Fatalf("post-stop double-counted emergency: %+v", got)
	}
}

func TestPostStopRecoversControlPlaneBeforeRecordingExit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task-recovery.json")
	recovered := false
	err := recoverAndRecordServiceExit(func() error {
		if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("service-exit marker existed before control-plane recovery: %v", statErr)
		}
		recovered = true
		return nil
	}, path, "exit-code", "75", "invocation-post-stop", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !recovered {
		t.Fatal("post-stop skipped control-plane recovery")
	}
	if _, present, loadErr := loadTaskRecoveryMarker(path); loadErr != nil || !present {
		t.Fatalf("service-exit marker after recovery: present=%v err=%v", present, loadErr)
	}
}

func TestPostStopThawFailurePreventsExitRecording(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task-recovery.json")
	wantErr := errors.New("thaw failed")
	err := recoverAndRecordServiceExit(func() error { return wantErr }, path, "exit-code", "75", "invocation-post-stop", time.Now())
	if !errors.Is(err, wantErr) {
		t.Fatalf("post-stop error=%v, want %v", err, wantErr)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed thaw still recorded service exit: %v", statErr)
	}
}

func TestRecordServiceExitClassifiesUnexpectedHandoffTruth(t *testing.T) {
	for _, test := range []struct {
		name           string
		handoffPresent bool
		want           string
	}{
		{name: "no handoff", want: "no_handoff"},
		{name: "preexisting handoff", handoffPresent: true, want: "preexisting_handoff"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "task-recovery.json")
			if err := recordServiceExitWithHandoff(path, "signal", "9", "unexpected-invocation", time.Now(), test.handoffPresent); err != nil {
				t.Fatal(err)
			}
			marker, present, err := loadTaskRecoveryMarker(path)
			if err != nil || !present || marker.ContinuityOutcome != test.want {
				t.Fatalf("marker=%+v present=%v err=%v, want continuity %q", marker, present, err, test.want)
			}
		})
	}
}

func TestRecordServiceExitProgressUncertainAdvancesGlobal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task-recovery.json")
	marker := taskRecoveryMarker{
		SchemaVersion:           taskRecoveryMarkerSchema,
		Timestamp:               time.Now().UTC(),
		ReasonCode:              pressure.ReasonHighWater,
		Generation:              1,
		ActiveOwner:             "app:namek",
		ActiveOwnerInvocationID: "invocation-2",
	}
	if err := writeTaskRecoveryMarker(path, marker); err != nil {
		t.Fatal(err)
	}
	if err := recordServiceExit(path, "exit-code", "76", "invocation-2", time.Now()); err != nil {
		t.Fatal(err)
	}
	got, _, _ := loadTaskRecoveryMarker(path)
	if got.Generation != 2 || got.GlobalStrike != 1 || got.ActiveOwner != "" || len(got.Suspects) != 0 {
		t.Fatalf("progress-uncertain exit misattributed: %+v", got)
	}
}

func TestMalformedTaskRecoveryMarkerNormalizesToGlobalStrikeTwo(t *testing.T) {
	got := malformedTaskRecoveryMarker("marker_malformed", time.Now())
	if got.Generation != 2 || got.GlobalStrike != 2 || len(got.Suspects) != 0 {
		t.Fatalf("malformed normalization = %+v", got)
	}
}

func TestTaskRecoveryBackoffSchedules(t *testing.T) {
	standard := []time.Duration{0, 0, 10 * time.Minute, 30 * time.Minute, 2 * time.Hour, 6 * time.Hour, 6 * time.Hour}
	for strike, want := range standard {
		if got := taskRecoveryBackoff(strike); got != want {
			t.Fatalf("standard strike %d = %s, want %s", strike, got, want)
		}
	}
	unlock := []time.Duration{30 * time.Second, 30 * time.Second, 2 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	for strike, want := range unlock {
		if got := unlockChainRecoveryBackoff(strike); got != want {
			t.Fatalf("unlock strike %d = %s, want %s", strike, got, want)
		}
	}
}

func TestTaskRecoveryClearSuspectPreservesOthers(t *testing.T) {
	marker := taskRecoveryMarker{Suspects: []taskRecoverySuspect{
		{Owner: "storage", Strike: 2},
		{Owner: "app:namek", Strike: 3},
	}}
	if marker.suspectStrike("app:namek") != 3 || !marker.clearSuspect("storage") {
		t.Fatalf("unexpected marker before clear: %+v", marker)
	}
	if len(marker.Suspects) != 1 || marker.Suspects[0].Owner != "app:namek" {
		t.Fatalf("clear removed wrong suspect: %+v", marker)
	}
}

func TestTaskMarkerCensusRetainsImmediateOwnerWhenCensusIsLate(t *testing.T) {
	got := taskMarkerCensus(nil, "network")
	if got == nil || got.LifecycleOwner != "network" {
		t.Fatalf("fallback census = %+v", got)
	}
	original := &pressure.TaskCensus{Goroutines: 42}
	got = taskMarkerCensus(original, "app:namek")
	if got.LifecycleOwner != "app:namek" || original.LifecycleOwner != "" {
		t.Fatalf("merged=%+v original=%+v", got, original)
	}
	explicit := &pressure.TaskCensus{LifecycleOwner: "storage"}
	if got := taskMarkerCensus(explicit, "network"); got.LifecycleOwner != "storage" {
		t.Fatalf("explicit census owner replaced: %+v", got)
	}
}
