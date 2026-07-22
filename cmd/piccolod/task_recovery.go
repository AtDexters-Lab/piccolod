package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"piccolod/internal/autounlock"
	"piccolod/internal/fsutil"
	"piccolod/internal/resources/pressure"
)

const (
	taskRecoveryMarkerSchema        = 2
	taskRecoverySuspectLimit        = 8
	taskEmergencyExitCode           = 75
	taskProgressUncertainExitCode   = 76
	taskEmergencyDeadline           = 3 * time.Second
	taskContinuityPrepareBudget     = 2 * time.Second
	taskEmergencyFinalizationBudget = time.Second
	taskMarkerNormalWindow          = 10 * time.Minute
)

var taskRecoveryMarkerPath = "/run/piccolo/task-recovery.json"

type taskRecoveryMarker struct {
	SchemaVersion           int                   `json:"schema_version"`
	Timestamp               time.Time             `json:"timestamp"`
	DetectionAt             time.Time             `json:"detection_at,omitempty"`
	ReasonCode              string                `json:"reason_code"`
	ContinuityOutcome       string                `json:"continuity_outcome,omitempty"`
	TaskCurrent             *int64                `json:"task_current,omitempty"`
	TaskLimit               *int64                `json:"task_limit,omitempty"`
	Generation              int                   `json:"generation"`
	LastFailedInvocationID  string                `json:"last_failed_invocation_id,omitempty"`
	ActiveOwner             string                `json:"active_owner,omitempty"`
	ActiveOwnerInvocationID string                `json:"active_owner_invocation_id,omitempty"`
	Suspects                []taskRecoverySuspect `json:"suspects,omitempty"`
	GlobalStrike            int                   `json:"global_strike,omitempty"`
}

type taskRecoverySuspect struct {
	Owner  string `json:"owner"`
	Strike int    `json:"strike"`
}

func taskRecoveryBackoff(strike int) time.Duration {
	switch {
	case strike <= 1:
		return 0
	case strike == 2:
		return 10 * time.Minute
	case strike == 3:
		return 30 * time.Minute
	case strike == 4:
		return 2 * time.Hour
	default:
		return 6 * time.Hour
	}
}

func unlockChainRecoveryBackoff(strike int) time.Duration {
	switch {
	case strike <= 1:
		return 30 * time.Second
	case strike == 2:
		return 2 * time.Minute
	default:
		return 5 * time.Minute
	}
}

func taskRecoveryMarkerHasInitialBackoff(marker taskRecoveryMarker) bool {
	if taskRecoveryBackoff(marker.GlobalStrike) > 0 {
		return true
	}
	for _, suspect := range marker.Suspects {
		if suspect.Owner == taskRecoveryUnlockChainOwner {
			if unlockChainRecoveryBackoff(suspect.Strike) > 0 {
				return true
			}
			continue
		}
		if taskRecoveryBackoff(suspect.Strike) > 0 {
			return true
		}
	}
	return false
}

func (m taskRecoveryMarker) suspectStrike(owner string) int {
	for _, suspect := range m.Suspects {
		if suspect.Owner == owner {
			return suspect.Strike
		}
	}
	return 0
}

func (m *taskRecoveryMarker) clearSuspect(owner string) bool {
	for i := range m.Suspects {
		if m.Suspects[i].Owner != owner {
			continue
		}
		m.Suspects = append(m.Suspects[:i], m.Suspects[i+1:]...)
		return true
	}
	return false
}

func loadTaskRecoveryMarker(path string) (taskRecoveryMarker, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return taskRecoveryMarker{}, false, nil
	}
	if err != nil {
		return taskRecoveryMarker{}, true, err
	}
	var marker taskRecoveryMarker
	if err := json.Unmarshal(data, &marker); err != nil {
		return taskRecoveryMarker{}, true, fmt.Errorf("decode task recovery marker: %w", err)
	}
	if marker.SchemaVersion != taskRecoveryMarkerSchema || marker.Generation < 1 || marker.ReasonCode == "" ||
		marker.GlobalStrike < 0 || len(marker.Suspects) > taskRecoverySuspectLimit {
		return taskRecoveryMarker{}, true, fmt.Errorf("invalid task recovery marker")
	}
	seen := make(map[string]struct{}, len(marker.Suspects))
	for _, suspect := range marker.Suspects {
		if suspect.Owner == "" || suspect.Strike < 1 {
			return taskRecoveryMarker{}, true, fmt.Errorf("invalid task recovery suspect")
		}
		if _, duplicate := seen[suspect.Owner]; duplicate {
			return taskRecoveryMarker{}, true, fmt.Errorf("duplicate task recovery suspect")
		}
		seen[suspect.Owner] = struct{}{}
	}
	return marker, true, nil
}

func buildTaskRecoveryMarkerForInvocationAt(snapshot pressure.TaskSnapshot, census *pressure.TaskCensus, previous taskRecoveryMarker, invocationID string, now time.Time) taskRecoveryMarker {
	if invocationID != "" && previous.LastFailedInvocationID == invocationID {
		return previous
	}
	owner := ""
	if census != nil {
		owner = census.LifecycleOwner
	}
	marker := advanceTaskRecoveryFailure(previous, owner, invocationID, snapshot.ReasonCode, now)
	if !snapshot.SampledAt.IsZero() {
		marker.DetectionAt = snapshot.SampledAt.UTC()
	}
	if snapshot.CurrentKnown {
		current := snapshot.Current
		marker.TaskCurrent = &current
	}
	if snapshot.LimitKnown {
		limit := snapshot.Limit
		marker.TaskLimit = &limit
	}
	return marker
}

func advanceTaskRecoveryFailure(previous taskRecoveryMarker, owner, invocationID, reason string, now time.Time) taskRecoveryMarker {
	if invocationID != "" && previous.LastFailedInvocationID == invocationID {
		return previous
	}
	marker := previous
	marker.SchemaVersion = taskRecoveryMarkerSchema
	marker.Timestamp = now.UTC()
	marker.DetectionAt = now.UTC()
	marker.ReasonCode = reason
	marker.ContinuityOutcome = ""
	marker.TaskCurrent = nil
	marker.TaskLimit = nil
	if marker.ReasonCode == "" {
		marker.ReasonCode = "service_failure"
	}
	marker.Generation++
	if marker.Generation < 1 {
		marker.Generation = 1
	}
	marker.LastFailedInvocationID = invocationID
	marker.ActiveOwner = ""
	marker.ActiveOwnerInvocationID = ""
	marker.Suspects = append([]taskRecoverySuspect(nil), previous.Suspects...)

	if owner == "" {
		marker.GlobalStrike++
		return marker
	}
	for i := range marker.Suspects {
		if marker.Suspects[i].Owner == owner {
			marker.Suspects[i].Strike++
			return marker
		}
	}
	if len(marker.Suspects) >= taskRecoverySuspectLimit {
		marker.GlobalStrike++
		return marker
	}
	marker.Suspects = append(marker.Suspects, taskRecoverySuspect{Owner: owner, Strike: 1})
	return marker
}

func malformedTaskRecoveryMarker(reason string, now time.Time) taskRecoveryMarker {
	if reason == "" {
		reason = "marker_malformed"
	}
	return taskRecoveryMarker{
		SchemaVersion: taskRecoveryMarkerSchema,
		Timestamp:     now.UTC(),
		DetectionAt:   now.UTC(),
		ReasonCode:    reason,
		Generation:    2,
		GlobalStrike:  2,
	}
}

func writeTaskRecoveryMarker(path string, marker taskRecoveryMarker) error {
	data, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return fsutil.AtomicWriteFile(path, data, 0o600)
}

func recordServiceExit(path, serviceResult, exitStatus, invocationID string, now time.Time) error {
	return recordServiceExitWithHandoff(path, serviceResult, exitStatus, invocationID, now, autounlock.BlobExists())
}

func recordServiceExitWithHandoff(path, serviceResult, exitStatus, invocationID string, now time.Time, handoffPresent bool) error {
	// ExecStopPost also runs after a clean operator stop. Clean exits are not
	// recovery evidence and must not manufacture a marker or a strike.
	if serviceResult == "success" && (exitStatus == "" || exitStatus == "0") {
		return nil
	}

	marker, present, err := loadTaskRecoveryMarker(path)
	if err != nil {
		marker = malformedTaskRecoveryMarker("marker_malformed", now)
		marker.LastFailedInvocationID = invocationID
		return writeTaskRecoveryMarker(path, marker)
	}
	if !present {
		marker = taskRecoveryMarker{}
	}
	if invocationID != "" && marker.LastFailedInvocationID == invocationID {
		// The in-process emergency owner already advanced this exact systemd
		// invocation. Preserve it rather than double-counting in ExecStopPost.
		return nil
	}

	owner := ""
	reason := "service_failure"
	if exitStatus == fmt.Sprint(taskProgressUncertainExitCode) {
		reason = "progress_state_uncertain"
	} else if marker.ActiveOwnerInvocationID == invocationID {
		owner = marker.ActiveOwner
	}
	marker = advanceTaskRecoveryFailure(marker, owner, invocationID, reason, now)
	if handoffPresent {
		marker.ContinuityOutcome = "preexisting_handoff"
	} else {
		marker.ContinuityOutcome = "no_handoff"
	}
	return writeTaskRecoveryMarker(path, marker)
}

func recoverAndRecordServiceExit(
	recoverControlPlane func() error,
	path, serviceResult, exitStatus, invocationID string,
	now time.Time,
) error {
	if recoverControlPlane != nil {
		if err := recoverControlPlane(); err != nil {
			return fmt.Errorf("recover pending control-plane thaw: %w", err)
		}
	}
	return recordServiceExit(path, serviceResult, exitStatus, invocationID, now)
}

func taskMarkerCensus(census *pressure.TaskCensus, fallbackOwner string) *pressure.TaskCensus {
	if census == nil {
		if fallbackOwner == "" {
			return nil
		}
		return &pressure.TaskCensus{LifecycleOwner: fallbackOwner}
	}
	copy := *census
	if copy.LifecycleOwner == "" {
		copy.LifecycleOwner = fallbackOwner
	}
	return &copy
}
