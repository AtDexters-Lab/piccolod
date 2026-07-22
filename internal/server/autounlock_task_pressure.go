package server

import (
	"context"
	"log"

	"piccolod/internal/autounlock"
	"piccolod/internal/resources/pressure"
)

// taskPressureRestartContinuityAdapter is the composition-root boundary
// between the task sampler and provider-neutral restart-unlock continuity.
// The pressure relay owns ordering and generation; the autounlock orchestrator
// owns serialization and all handoff/provider decisions.
type taskPressureRestartContinuityAdapter struct {
	ctx        context.Context
	reconciler autounlock.TaskPressureIntentReconciler
}

func (a taskPressureRestartContinuityAdapter) ApplyTaskPressureIntent(
	intent pressure.RestartContinuityIntent,
	latest pressure.RestartContinuityIntentView,
) {
	if a.reconciler == nil {
		return
	}
	ctx := a.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	mappedLatest := autounlock.TaskPressureIntentView(func() autounlock.TaskPressureIntent {
		return mapTaskPressureIntent(latest.Latest())
	})
	if err := a.reconciler.ReconcileTaskPressureIntent(ctx, mapTaskPressureIntent(intent), mappedLatest); err != nil {
		log.Printf("WARN: restart-unlock continuity pressure reconcile failed: %v", err)
	}
}

func mapTaskPressureIntent(intent pressure.RestartContinuityIntent) autounlock.TaskPressureIntent {
	return autounlock.TaskPressureIntent{
		State:      autounlock.TaskPressureIntentState(intent.State),
		Generation: intent.Generation,
	}
}

func attachTaskPressureRestartContinuity(
	ctx context.Context,
	guard *pressure.TaskGuard,
	reconciler autounlock.TaskPressureIntentReconciler,
) bool {
	if guard == nil || reconciler == nil {
		return false
	}
	guard.AttachRestartContinuity(taskPressureRestartContinuityAdapter{
		ctx:        ctx,
		reconciler: reconciler,
	})
	return true
}
