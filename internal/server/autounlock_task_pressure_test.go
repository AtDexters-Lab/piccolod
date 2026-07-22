package server

import (
	"context"
	"testing"

	"piccolod/internal/autounlock"
	"piccolod/internal/resources/pressure"
)

type taskPressureReconcilerFunc func(
	context.Context,
	autounlock.TaskPressureIntent,
	autounlock.TaskPressureIntentView,
) error

func (f taskPressureReconcilerFunc) ReconcileTaskPressureIntent(
	ctx context.Context,
	intent autounlock.TaskPressureIntent,
	latest autounlock.TaskPressureIntentView,
) error {
	return f(ctx, intent, latest)
}

func TestTaskPressureRestartContinuityAdapterMapsIntentAndLiveView(t *testing.T) {
	type contextKey struct{}
	ctx := context.WithValue(context.Background(), contextKey{}, "server")
	current := pressure.RestartContinuityIntent{
		State:      pressure.TaskPressureWarning,
		Generation: 7,
	}

	called := false
	adapter := taskPressureRestartContinuityAdapter{
		ctx: ctx,
		reconciler: taskPressureReconcilerFunc(func(
			gotCtx context.Context,
			requested autounlock.TaskPressureIntent,
			latest autounlock.TaskPressureIntentView,
		) error {
			called = true
			if gotCtx.Value(contextKey{}) != "server" {
				t.Fatal("adapter did not preserve server operation context")
			}
			if requested.State != autounlock.TaskPressureIntentWarning || requested.Generation != 7 {
				t.Fatalf("requested intent = %+v, want warning generation 7", requested)
			}

			current = pressure.RestartContinuityIntent{
				State:      pressure.TaskPressureCritical,
				Generation: 8,
			}
			if got := latest(); got.State != autounlock.TaskPressureIntentCritical || got.Generation != 8 {
				t.Fatalf("live latest intent = %+v, want critical generation 8", got)
			}
			return nil
		}),
	}

	adapter.ApplyTaskPressureIntent(
		pressure.RestartContinuityIntent{State: pressure.TaskPressureWarning, Generation: 7},
		pressure.RestartContinuityIntentView(func() pressure.RestartContinuityIntent { return current }),
	)
	if !called {
		t.Fatal("reconciler was not called")
	}
}

func TestMapTaskPressureIntentPreservesStateAndGeneration(t *testing.T) {
	tests := []struct {
		name string
		in   pressure.RestartContinuityIntent
		want autounlock.TaskPressureIntentState
	}{
		{name: "normal", in: pressure.RestartContinuityIntent{State: pressure.TaskPressureNormal, Generation: 1}, want: autounlock.TaskPressureIntentNormal},
		{name: "warning", in: pressure.RestartContinuityIntent{State: pressure.TaskPressureWarning, Generation: 2}, want: autounlock.TaskPressureIntentWarning},
		{name: "critical", in: pressure.RestartContinuityIntent{State: pressure.TaskPressureCritical, Generation: 3}, want: autounlock.TaskPressureIntentCritical},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mapTaskPressureIntent(tt.in)
			if got.State != tt.want || got.Generation != tt.in.Generation {
				t.Fatalf("mapped intent = %+v, want state %q generation %d", got, tt.want, tt.in.Generation)
			}
		})
	}
}

func TestTaskPressureRestartContinuityWiringAndServerAccessorHandleNil(t *testing.T) {
	guard := pressure.NewTaskGuard(pressure.TaskGuardConfig{Disabled: true})
	if attachTaskPressureRestartContinuity(context.Background(), guard, nil) {
		t.Fatal("nil reconciler must not be attached")
	}
	if attachTaskPressureRestartContinuity(context.Background(), nil, taskPressureReconcilerFunc(func(
		context.Context,
		autounlock.TaskPressureIntent,
		autounlock.TaskPressureIntentView,
	) error {
		return nil
	})) {
		t.Fatal("nil task guard must not report attached")
	}

	var nilServer *GinServer
	if got := nilServer.RestartUnlockContinuity(); got != nil {
		t.Fatalf("nil server continuity = %T, want nil", got)
	}
	if got := (&GinServer{}).RestartUnlockContinuity(); got != nil {
		t.Fatalf("server without orchestrator continuity = %T, want nil", got)
	}
}
