package server

import (
	"context"
	"errors"

	"piccolod/internal/autounlock"
	"piccolod/internal/update"
)

// errUpdateManagerUnavailable is returned by osUpdateManagerAdapter.Reboot
// when the inner update manager is nil. Distinct from a successful no-op so
// the scheduler audits the misconfiguration via scheduler.failed instead of
// silently flipping last_fired_at + alreadyTriedThisWindow without ever
// rebooting (which would suppress every subsequent fire until tEdge rollover).
var errUpdateManagerUnavailable = errors.New("autounlock adapter: update manager unavailable")

// osUpdateManagerAdapter bridges the autounlock.UpdateManager interface to the
// existing GinServer.updateManager surface. It maps update's richer snapshot
// state into autounlock's small readiness enum so the scheduler can distinguish
// absent, in-progress, unknown, and staged states.
//
// `inner` is captured by closure so the value is read at call time —
// updateManager is set during NewGinServer AFTER autounlock construction in
// the auto-init path, so an early-bound pointer would observe nil.
type osUpdateManagerAdapter struct {
	inner func() osUpdateManager
}

func (a *osUpdateManagerAdapter) UpdateReadiness(ctx context.Context) (autounlock.UpdateReadiness, error) {
	m := a.inner()
	if m == nil {
		return autounlock.UpdateReadinessUnknown, errUpdateManagerUnavailable
	}
	st, err := m.SnapshotState(ctx)
	if err != nil {
		return autounlock.UpdateReadinessUnknown, err
	}
	switch st.Readiness {
	case update.SnapshotReadinessStaged:
		return autounlock.UpdateReadinessStaged, nil
	case update.SnapshotReadinessAbsent:
		return autounlock.UpdateReadinessAbsent, nil
	case update.SnapshotReadinessInProgress:
		return autounlock.UpdateReadinessInProgress, nil
	default:
		return autounlock.UpdateReadinessUnknown, nil
	}
}

func (a *osUpdateManagerAdapter) Reboot(ctx context.Context) error {
	m := a.inner()
	if m == nil {
		return errUpdateManagerUnavailable
	}
	return m.Reboot(ctx)
}
