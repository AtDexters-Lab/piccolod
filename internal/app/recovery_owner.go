package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"piccolod/internal/resources/pressure"
)

const recoveryReconcileLockRetryInterval = 10 * time.Millisecond

var (
	// ErrRecoveryDeadlineRequired prevents an automatic recovery owner from
	// entering lifecycle code without the liveness bound required by the task
	// recovery controller.
	ErrRecoveryDeadlineRequired = errors.New("app recovery: finite deadline required")
	// ErrRecoveryAppNotDesired reports that durable desire changed after the
	// caller took its deterministic owner snapshot.
	ErrRecoveryAppNotDesired = errors.New("app recovery: app is no longer desired")
	// ErrRecoveryObservationUnknown distinguishes a safe no-effect observation
	// result from a successfully recovered owner.
	ErrRecoveryObservationUnknown = errors.New("app recovery: runtime observation unknown")
	// ErrRecoveryTransitionPending reports that a durable transition still owns
	// the app and ordinary reconcile therefore did not run.
	ErrRecoveryTransitionPending = errors.New("app recovery: durable transition remains pending")
)

// DesiredAppRecoveryOwner is the durable identity and route shape of one
// enabled app selected for serialized startup recovery. RouteBearing is only
// scheduling metadata; RecoverDesiredApp re-reads current durable truth before
// it reports a stability proof.
type DesiredAppRecoveryOwner struct {
	InstanceID   string
	RouteBearing bool
}

// AppRecoveryResult is the typed proof returned by one serialized automatic
// app recovery attempt. Recovered requires a complete known observation and a
// successful reconcile. Route-bearing apps additionally need
// ActivePublication before the process-level controller may treat the owner as
// stable; listenerless workspaces have no route to publish.
type AppRecoveryResult struct {
	InstanceID        string
	Recovered         bool
	RouteBearing      bool
	ActivePublication bool
}

// StabilityProven keeps route publication fail-closed without making an empty
// listener set an impossible recovery condition.
func (r AppRecoveryResult) StabilityProven() bool {
	return r.Recovered && (!r.RouteBearing || r.ActivePublication)
}

// ObserveDesiredAppRecoveryActive reports whether an already-reacquired app
// still satisfies the stability proof used by task-recovery strike clearing.
// It is deliberately observe-only: no container inspection, reconcile, route
// mutation, or durable-state write is allowed from this path. If another app
// lifecycle operation currently owns reconciliation, the observation is
// unknown and therefore fails closed for the current stability interval.
func (m *AppManager) ObserveDesiredAppRecoveryActive(ctx context.Context, instanceID string) (bool, error) {
	instanceID = strings.TrimSpace(instanceID)
	if m == nil {
		return false, fmt.Errorf("%w: app manager unavailable", ErrRecoveryObservationUnknown)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if instanceID == "" {
		return false, fmt.Errorf("%w: empty instance id", ErrRecoveryAppNotDesired)
	}
	if !m.reconcileMu.TryLock() {
		return false, fmt.Errorf("%w: app lifecycle is busy", ErrRecoveryObservationUnknown)
	}
	defer m.reconcileMu.Unlock()

	if err := m.ensureUnlocked(); err != nil {
		return false, err
	}
	state, err := m.ensureStateManager()
	if err != nil {
		return false, err
	}
	appInst, exists := state.GetApp(instanceID)
	if !exists || appInst == nil || !appInst.Enabled {
		return false, fmt.Errorf("%w: %s", ErrRecoveryAppNotDesired, instanceID)
	}
	if err := m.rejectIfTransitionInProgress(state, instanceID, TransitionFenceNormalReconcile); err != nil {
		return false, err
	}
	if m.appObservationUnknown(instanceID) {
		return false, fmt.Errorf("%w: %s", ErrRecoveryObservationUnknown, instanceID)
	}
	if m.getObservedStatus(instanceID) != StatusRunning {
		return false, nil
	}
	if appInst.Definition == nil {
		return false, fmt.Errorf("%w: app definition unavailable for %s", ErrRecoveryObservationUnknown, instanceID)
	}
	if len(appInst.Definition.Listeners) == 0 {
		return true, nil
	}
	if m.serviceManager == nil {
		return false, fmt.Errorf("%w: publication manager unavailable for %s", ErrRecoveryObservationUnknown, instanceID)
	}
	return m.serviceManager.AppPublicationActive(instanceID), nil
}

// DesiredRecoveryAppOwners snapshots durable Enabled app desire without
// changing it. Stable identifier order lets the startup controller select the
// RFC first-route candidate and later give every remaining app a fresh bound.
func (m *AppManager) DesiredRecoveryAppOwners(ctx context.Context) ([]DesiredAppRecoveryOwner, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := m.lockRecoveryReconcile(ctx); err != nil {
		return nil, err
	}
	defer m.reconcileMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := m.ensureUnlocked(); err != nil {
		return nil, err
	}
	state, err := m.ensureStateManager()
	if err != nil {
		return nil, err
	}

	owners := make([]DesiredAppRecoveryOwner, 0)
	for _, appInst := range state.ListApps() {
		if appInst == nil || !appInst.Enabled {
			continue
		}
		instanceID := strings.TrimSpace(appInst.InstanceID)
		if instanceID == "" {
			continue
		}
		owners = append(owners, DesiredAppRecoveryOwner{
			InstanceID:   instanceID,
			RouteBearing: appInst.Definition != nil && len(appInst.Definition.Listeners) > 0,
		})
	}
	sort.Slice(owners, func(i, j int) bool {
		return owners[i].InstanceID < owners[j].InstanceID
	})
	return owners, nil
}

// lockRecoveryReconcile lets bounded recovery discovery abandon lifecycle
// serialization when its caller deadline expires. Ordinary lifecycle methods
// retain their existing blocking lock semantics.
func (m *AppManager) lockRecoveryReconcile(ctx context.Context) error {
	for {
		if m.reconcileMu.TryLock() {
			return nil
		}
		timer := time.NewTimer(recoveryReconcileLockRetryInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// RecoverDesiredApp performs exactly one app owner's startup recovery under
// the existing lifecycle serialization and admission policy. It does not scan
// or reconcile any other app. The caller supplies a fresh finite context for
// each owner (five seconds for the first-route qualification candidate).
func (m *AppManager) RecoverDesiredApp(ctx context.Context, instanceID string) (AppRecoveryResult, error) {
	result := AppRecoveryResult{InstanceID: strings.TrimSpace(instanceID)}
	if ctx == nil {
		return result, ErrRecoveryDeadlineRequired
	}
	if _, ok := ctx.Deadline(); !ok {
		return result, ErrRecoveryDeadlineRequired
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if result.InstanceID == "" {
		return result, fmt.Errorf("%w: empty instance id", ErrRecoveryAppNotDesired)
	}

	releaseOwner := pressure.BeginLifecycleOwner("app:" + result.InstanceID)
	defer releaseOwner()
	if err := m.lockRecoveryReconcile(ctx); err != nil {
		return result, err
	}
	defer m.reconcileMu.Unlock()
	if err := ctx.Err(); err != nil {
		return result, err
	}

	if err := m.ensureUnlocked(); err != nil {
		return result, err
	}
	if err := m.ensureKernelLeader(); err != nil {
		return result, err
	}
	state, err := m.ensureStateManager()
	if err != nil {
		return result, err
	}
	appInst, exists := state.GetApp(result.InstanceID)
	if !exists || appInst == nil || !appInst.Enabled {
		return result, fmt.Errorf("%w: %s", ErrRecoveryAppNotDesired, result.InstanceID)
	}
	transitionCtx := withTransitionRecoveryAdmission(ctx)
	if blocked, recoveryErr := m.recoverDesiredAppTransition(transitionCtx, state, appInst); recoveryErr != nil {
		m.setObservedStatus(result.InstanceID, StatusError)
		return result, recoveryErr
	} else if blocked {
		return result, fmt.Errorf("%w: %s", ErrRecoveryTransitionPending, result.InstanceID)
	}
	if transitionRecoveryMustYield(transitionCtx) {
		return result, fmt.Errorf("%w: %s yielded after admitted transition", ErrRecoveryTransitionPending, result.InstanceID)
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	currentDefinition, err := state.GetAppDefinition(result.InstanceID)
	if err != nil {
		return result, err
	}
	result.RouteBearing = len(currentDefinition.Listeners) > 0

	if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkLifecycle); err != nil {
		return result, err
	}
	if err := m.rejectIfTransitionInProgress(state, result.InstanceID, TransitionFenceNormalReconcile); err != nil {
		return result, err
	}

	m.beginObservationPass()
	if err := m.reconcileApp(ctx, state, appInst); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkLifecycle); err != nil {
		return result, err
	}
	if m.appObservationUnknown(result.InstanceID) {
		return result, fmt.Errorf("%w: %s", ErrRecoveryObservationUnknown, result.InstanceID)
	}
	if m.serviceManager != nil {
		result.ActivePublication = m.serviceManager.AppPublicationActive(result.InstanceID)
	}
	result.Recovered = m.getObservedStatus(result.InstanceID) == StatusRunning
	return result, nil
}

// recoverDesiredAppTransition recovers only appInst's authoritative/legacy
// transition state. The shared continuation token preserves the existing
// Warning behavior while preventing this one-owner entry point from wandering
// into another app.
func (m *AppManager) recoverDesiredAppTransition(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance) (bool, error) {
	instanceID := appInst.InstanceID
	record, err := state.LoadTransitionRecord(instanceID)
	switch {
	case err == nil:
		if record == nil {
			return true, errors.New("read authoritative transition record: empty record")
		}
		recoveryCtx, admitted := admitPendingTransitionRecovery(ctx)
		if !admitted {
			return true, admissionFailure(ctx)
		}
		if m.recoverPendingTransitionRecord(recoveryCtx, state, appInst, record) {
			return true, nil
		}
		if transitionRecoveryMustYield(recoveryCtx) {
			return true, nil
		}
	case !errors.Is(err, os.ErrNotExist):
		return true, fmt.Errorf("read authoritative transition record: %w", err)
	}

	imageTxn, err := state.LoadImageUpdateTransaction(instanceID)
	if err == nil {
		recoveryCtx, admitted := admitPendingTransitionRecovery(ctx)
		if !admitted {
			return true, admissionFailure(ctx)
		}
		if recoverErr := m.recoverOneImageUpdate(recoveryCtx, state, appInst, imageTxn); recoverErr != nil {
			return true, fmt.Errorf("image update recovery: %w", recoverErr)
		}
		if transitionRecoveryMustYield(recoveryCtx) {
			return true, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return true, fmt.Errorf("image update recovery: load transaction: %w", err)
	}
	if ctx.Err() != nil {
		return true, ctx.Err()
	}

	manifestTxn, err := state.LoadManifestUpdateTransaction(instanceID)
	if err == nil {
		recoveryCtx, admitted := admitPendingTransitionRecovery(ctx)
		if !admitted {
			return true, admissionFailure(ctx)
		}
		if recoverErr := m.recoverOneManifestUpdate(recoveryCtx, state, appInst, manifestTxn); recoverErr != nil {
			return true, fmt.Errorf("manifest update recovery: %w", recoverErr)
		}
		if transitionRecoveryMustYield(recoveryCtx) {
			return true, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return true, fmt.Errorf("manifest update recovery: load transaction: %w", err)
	}
	return false, nil
}

func admissionFailure(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkLifecycle); err != nil {
		return err
	}
	return ErrRecoveryTransitionPending
}

func (m *AppManager) appObservationUnknown(instanceID string) bool {
	m.unknownObservationMu.Lock()
	_, unknown := m.unknownObservations[instanceID]
	m.unknownObservationMu.Unlock()
	return unknown
}
