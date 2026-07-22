package autounlock

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// TaskPressureIntentState is the provider-neutral pressure vocabulary consumed
// by restart-unlock continuity. It intentionally does not import the task
// sampler package; the server composition root maps the sampler's snapshot
// into this small capability boundary.
type TaskPressureIntentState string

const (
	TaskPressureIntentNormal   TaskPressureIntentState = "normal"
	TaskPressureIntentWarning  TaskPressureIntentState = "warning"
	TaskPressureIntentCritical TaskPressureIntentState = "critical"
)

// TaskPressureIntent identifies one monotonic desired-state transition.
// Generation is supplied by the task-pressure owner and starts at one.
type TaskPressureIntent struct {
	State      TaskPressureIntentState
	Generation uint64
}

// TaskPressureIntentView returns the pressure owner's live latest intent. The
// orchestrator reads it only while holding its operation gate, including once
// after a potentially blocking Warning deposit, so queued callbacks cannot
// mutate the handoff from an obsolete snapshot.
type TaskPressureIntentView func() TaskPressureIntent

// TaskPressureIntentReconciler is the capability wired to the task-pressure
// relay by the server composition root.
type TaskPressureIntentReconciler interface {
	ReconcileTaskPressureIntent(context.Context, TaskPressureIntent, TaskPressureIntentView) error
}

var ErrInvalidTaskPressureIntent = errors.New("autounlock: invalid task-pressure intent")

const taskWarningHandoffTTL = 10 * time.Minute

// ReconcileTaskPressureIntent atomically reconciles task-pressure intent with
// the local restart handoff. Provider work remains serialized with ceremony,
// pickup, settings, test, and emergency preparation through the existing
// operation gate.
//
// Warning prepares a ten-minute handoff. Normal removes only local authority;
// the provider factor is deliberately left to expire. Critical performs no
// provider I/O because the process-level fatal owner owns its bounded
// last-chance Prepare call.
func (o *Orchestrator) ReconcileTaskPressureIntent(
	ctx context.Context,
	requested TaskPressureIntent,
	latest TaskPressureIntentView,
) error {
	if err := validateTaskPressureIntent(requested); err != nil {
		return err
	}
	if latest == nil {
		return fmt.Errorf("%w: latest-state view is nil", ErrInvalidTaskPressureIntent)
	}
	if err := o.acquire(ctx); err != nil {
		return err
	}
	defer o.release()

	current, err := currentTaskPressureIntent(requested, latest)
	if err != nil {
		return err
	}
	switch current.State {
	case TaskPressureIntentNormal:
		return o.cancelTaskWarningHandoffLocked()
	case TaskPressureIntentCritical:
		return nil
	case TaskPressureIntentWarning:
		_, prepareErr := o.prepareLocked(ctx, PrepareTriggerTaskWarning, taskWarningHandoffTTL)

		// Deposit may have blocked while pressure advanced. Reconcile that
		// latest state before another continuity operation can acquire the
		// gate. Critical retains whatever Warning managed to prepare; its
		// process owner may reuse it without a second provider deposit.
		after, latestErr := currentTaskPressureIntent(current, latest)
		if latestErr != nil {
			return errors.Join(prepareErr, latestErr)
		}
		if after.State == TaskPressureIntentNormal {
			return errors.Join(prepareErr, o.cancelTaskWarningHandoffLocked())
		}
		return prepareErr
	default:
		panic("unreachable task-pressure intent state")
	}
}

func currentTaskPressureIntent(requested TaskPressureIntent, latest TaskPressureIntentView) (TaskPressureIntent, error) {
	current := latest()
	if err := validateTaskPressureIntent(current); err != nil {
		return TaskPressureIntent{}, err
	}
	if current.Generation < requested.Generation {
		return TaskPressureIntent{}, fmt.Errorf(
			"%w: latest generation %d precedes requested generation %d",
			ErrInvalidTaskPressureIntent,
			current.Generation,
			requested.Generation,
		)
	}
	return current, nil
}

func validateTaskPressureIntent(intent TaskPressureIntent) error {
	if intent.Generation == 0 {
		return fmt.Errorf("%w: generation must be non-zero", ErrInvalidTaskPressureIntent)
	}
	switch intent.State {
	case TaskPressureIntentNormal, TaskPressureIntentWarning, TaskPressureIntentCritical:
		return nil
	default:
		return fmt.Errorf("%w: unknown state %q", ErrInvalidTaskPressureIntent, intent.State)
	}
}

// cancelTaskWarningHandoffLocked removes local authority only when Warning
// created the exact current blob. Pre-existing handoffs are not Warning-owned,
// and a matching graceful/fatal restart claim takes precedence. Namek v1
// intentionally has no revoke: deleting the owned raw blob first leaves an
// inert remote factor that can only expire.
func (o *Orchestrator) cancelTaskWarningHandoffLocked() error {
	blob, err := ReadBlob()
	if err != nil {
		if !errors.Is(err, ErrBlobMissing) {
			return err
		}
		o.clearHandoffClaimsLocked()
		state, _ := LoadState()
		return o.clearLocalHandoffLocked(&state)
	}
	digest := rawBlobDigest(blob)
	if o.restartHandoffClaimDigest == digest {
		return nil
	}
	if o.restartHandoffClaimDigest != "" {
		o.restartHandoffClaimDigest = ""
	}
	if o.taskWarningHandoffClaimDigest != digest {
		// Warning ownership never transfers to pre-existing or replacement bytes.
		o.taskWarningHandoffClaimDigest = ""
		return nil
	}
	state, _ := LoadState()
	return o.clearLocalHandoffLocked(&state)
}
