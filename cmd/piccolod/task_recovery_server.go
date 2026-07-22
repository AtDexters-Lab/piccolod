package main

import (
	"context"
	"strings"

	"piccolod/internal/resources/pressure"
	"piccolod/internal/server"
)

// ginTaskRecoveryRuntime adapts server-owned lifecycle capabilities to the
// cmd-owned marker/scheduling runner. It carries no marker state into server.
type ginTaskRecoveryRuntime struct {
	server *server.GinServer
}

func (r ginTaskRecoveryRuntime) TaskPressureSnapshot() pressure.TaskSnapshot {
	if r.server == nil {
		return pressure.TaskSnapshot{State: pressure.TaskPressureUnavailable, ReasonCode: pressure.ReasonMonitorUnavailable}
	}
	return r.server.TaskPressureSnapshot()
}

func (r ginTaskRecoveryRuntime) LifecycleReady() bool {
	return r.server != nil && r.server.LifecycleReady()
}

func (r ginTaskRecoveryRuntime) CoreTaskRecoveryOwners() []taskRecoveryOwner {
	if r.server == nil {
		return nil
	}
	return adaptServerTaskRecoveryOwners(r.server.CoreTaskRecoveryOwners())
}

func (r ginTaskRecoveryRuntime) DecryptedTaskRecoveryOwners(ctx context.Context) ([]taskRecoveryOwner, error) {
	if r.server == nil {
		return nil, nil
	}
	owners, err := r.server.DecryptedTaskRecoveryOwners(ctx)
	if err != nil {
		return nil, err
	}
	return adaptServerTaskRecoveryOwners(owners), nil
}

func (r ginTaskRecoveryRuntime) PrepareTaskRecoveryApps(instanceIDs []string) {
	if r.server != nil {
		r.server.PrepareTaskRecoveryApps(instanceIDs)
	}
}

func (r ginTaskRecoveryRuntime) ReleaseTaskRecoveryApp(instanceID string) {
	if r.server != nil {
		r.server.ReleaseTaskRecoveryApp(instanceID)
	}
}

func (r ginTaskRecoveryRuntime) SetTaskRecoveryGlobalSuppression(suppressed bool) {
	if r.server != nil {
		r.server.SetTaskRecoveryGlobalSuppression(suppressed)
	}
}

func adaptServerTaskRecoveryOwners(owners []server.TaskRecoveryOwner) []taskRecoveryOwner {
	adapted := make([]taskRecoveryOwner, 0, len(owners))
	for _, owner := range owners {
		appID := ""
		if strings.HasPrefix(owner.Name, "app:") {
			appID = strings.TrimSpace(strings.TrimPrefix(owner.Name, "app:"))
		}
		adapted = append(adapted, taskRecoveryOwner{
			Name:          owner.Name,
			AppID:         appID,
			Timeout:       owner.Timeout,
			Attempt:       owner.Attempt,
			ObserveActive: owner.ObserveActive,
		})
		if owner.AttemptWithResult != nil {
			adapted[len(adapted)-1].AttemptDetailed = func(ctx context.Context) taskRecoveryOwnerAttemptResult {
				result, err := owner.AttemptWithResult(ctx)
				return taskRecoveryOwnerAttemptResult{
					Active: result.Active, RouteKnown: true,
					RouteActive: result.RouteBearing && result.ActivePublication,
					Err:         err,
				}
			}
		}
		if owner.RouteQualification != nil {
			adapted[len(adapted)-1].RouteQualification = &taskRecoveryQualification{
				Timeout: owner.RouteQualification.Timeout,
				Attempt: owner.RouteQualification.Attempt,
			}
		}
	}
	return adapted
}
