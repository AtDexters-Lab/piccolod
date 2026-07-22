package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"piccolod/internal/app"
	"piccolod/internal/autounlock"
	"piccolod/internal/resources/pressure"
	"piccolod/internal/storage"
)

const (
	taskRecoveryUnlockTimeout  = 20 * time.Second
	taskRecoveryKeyslotTimeout = 2 * time.Minute
	taskRecoveryStorageTimeout = 5 * time.Minute
	taskRecoveryCatalogTimeout = 30 * time.Second
	taskRecoveryFirstRoute     = 5 * time.Second
	taskRecoveryAppTimeout     = 30 * time.Second
	taskRecoveryNetworkTimeout = 30 * time.Second
	taskRecoveryUpdateTimeout  = time.Minute
)

// TaskRecoveryQualification is the optional, stricter first-route proof for
// one app owner. The process-level runner may freeze exactly one qualification
// from its initial decrypted snapshot; later enumerations cannot transfer it.
type TaskRecoveryQualification struct {
	Timeout time.Duration
	Attempt func(context.Context) (active bool, err error)
}

// TaskRecoveryOwnerResult preserves the fresh route/publication truth from an
// app recovery attempt instead of forcing the process runner to infer it from
// enumeration-time metadata.
type TaskRecoveryOwnerResult struct {
	Active            bool
	RouteBearing      bool
	ActivePublication bool
}

// TaskRecoveryOwner is the marker-free capability boundary consumed by the
// process-level recovery controller. Timeout and Attempt/AttemptWithResult
// always describe the ordinary convergence pass. App owners use the detailed
// result to preserve fresh route/publication truth and also expose a
// side-effect-free current activity observation so cmd can require continuous
// post-reacquisition
// status/publication. RouteQualification is explicit metadata for the initial
// first-route slice; cmd owns its run-wide selection as well as deadline/grace/
// fatal arbitration and marker acknowledgement.
type TaskRecoveryOwner struct {
	Name               string
	Timeout            time.Duration
	Attempt            func(context.Context) (active bool, err error)
	AttemptWithResult  func(context.Context) (TaskRecoveryOwnerResult, error)
	ObserveActive      func(context.Context) (active bool, err error)
	RouteQualification *TaskRecoveryQualification
}

// LifecycleReady reports the one authoritative post-decrypt readiness state.
func (s *GinServer) LifecycleReady() bool {
	return s != nil && s.lifecycle != nil && s.lifecycle.IsReady()
}

// CoreTaskRecoveryOwners returns only owners that can be entered while the
// locked access plane is serving. Unlock remains first so long storage work
// cannot consume the unattended recovery window.
func (s *GinServer) CoreTaskRecoveryOwners() []TaskRecoveryOwner {
	if s == nil {
		return nil
	}
	owners := []TaskRecoveryOwner{s.unlockTaskRecoveryOwner()}
	if s.keyslotReconciler != nil {
		owners = append(owners, s.keyslotTaskRecoveryOwner())
	}
	if s.storageMgr != nil && s.onboardingMgr != nil {
		owners = append(owners, s.storageTaskRecoveryOwner())
	}
	return owners
}

// DecryptedTaskRecoveryOwners snapshots post-Ready durable desire. It places
// the deterministic route-qualification candidate first, then retains stable
// identifier order. App closures capture exactly one normalized identifier;
// each invocation creates a fresh finite operation context.
func (s *GinServer) DecryptedTaskRecoveryOwners(ctx context.Context) ([]TaskRecoveryOwner, error) {
	if s == nil || !s.LifecycleReady() {
		return nil, errors.New("task recovery: decrypted owners require lifecycle Ready")
	}
	var appOwners []app.DesiredAppRecoveryOwner
	if s.appManager != nil {
		var err error
		appOwners, err = s.appManager.DesiredRecoveryAppOwners(ctx)
		if err != nil {
			return nil, err
		}
	}
	return s.decryptedTaskRecoveryOwners(appOwners, nil), nil
}

type recoverDesiredAppFunc func(context.Context, string) (app.AppRecoveryResult, error)

func (s *GinServer) decryptedTaskRecoveryOwners(appOwners []app.DesiredAppRecoveryOwner, recoverApp recoverDesiredAppFunc) []TaskRecoveryOwner {
	owners := make([]TaskRecoveryOwner, 0, len(appOwners)+3)
	if recoverApp == nil && s.appManager != nil {
		recoverApp = s.appManager.RecoverDesiredApp
	}
	if recoverApp != nil {
		orderedApps := orderedRecoveryAppOwners(appOwners)
		for index, recoveryApp := range orderedApps {
			instanceID := recoveryApp.InstanceID
			firstRouteCandidate := index == 0 && recoveryApp.RouteBearing
			attemptWithResult := func(parent context.Context) (TaskRecoveryOwnerResult, error) {
				ctx, cancel := taskRecoveryAttemptContext(parent, taskRecoveryAppTimeout)
				defer cancel()
				result, err := recoverApp(ctx, instanceID)
				return TaskRecoveryOwnerResult{
					Active:            strings.TrimSpace(result.InstanceID) == instanceID && result.StabilityProven(),
					RouteBearing:      result.RouteBearing,
					ActivePublication: result.ActivePublication,
				}, err
			}
			owner := TaskRecoveryOwner{
				Name:              "app:" + instanceID,
				Timeout:           taskRecoveryAppTimeout,
				AttemptWithResult: attemptWithResult,
				Attempt: func(parent context.Context) (bool, error) {
					result, err := attemptWithResult(parent)
					return result.Active, err
				},
				ObserveActive: func(ctx context.Context) (bool, error) {
					if s.appManager == nil {
						return false, errors.New("task recovery: app activity observer unavailable")
					}
					return s.appManager.ObserveDesiredAppRecoveryActive(ctx, instanceID)
				},
			}
			if firstRouteCandidate {
				owner.RouteQualification = &TaskRecoveryQualification{
					Timeout: taskRecoveryFirstRoute,
					Attempt: func(parent context.Context) (bool, error) {
						ctx, cancel := taskRecoveryAttemptContext(parent, taskRecoveryFirstRoute)
						defer cancel()
						result, err := recoverApp(ctx, instanceID)
						// Qualification is bound to the route-bearing app chosen
						// from this durable snapshot. A later shape change may be a
						// valid ordinary recovery result, but it cannot turn this
						// frozen route qualification into success.
						return strings.TrimSpace(result.InstanceID) == instanceID &&
							result.Recovered && result.RouteBearing && result.ActivePublication, err
					},
				}
			}
			owners = append(owners, owner)
		}
	}
	if s.catalogManager != nil {
		owners = append(owners, s.catalogTaskRecoveryOwner())
	}
	if s.networkSupervisor != nil {
		owners = append(owners, s.networkTaskRecoveryOwner())
	}
	if s.updateManager != nil {
		owners = append(owners, s.updateTaskRecoveryOwner())
	}
	return owners
}

// orderedRecoveryAppOwners makes the first-route qualification phase explicit:
// the lowest route-bearing app goes first, then all remaining apps retain
// stable identifier order. Listenerless-only fleets have no five-second route
// qualification candidate and still receive ordinary per-owner recovery.
func orderedRecoveryAppOwners(appOwners []app.DesiredAppRecoveryOwner) []app.DesiredAppRecoveryOwner {
	byID := make(map[string]app.DesiredAppRecoveryOwner, len(appOwners))
	for _, owner := range appOwners {
		instanceID := strings.TrimSpace(owner.InstanceID)
		if instanceID == "" {
			continue
		}
		owner.InstanceID = instanceID
		if existing, duplicate := byID[instanceID]; duplicate {
			// Conflicting duplicate snapshots fail toward route qualification.
			owner.RouteBearing = owner.RouteBearing || existing.RouteBearing
		}
		byID[instanceID] = owner
	}
	ordered := make([]app.DesiredAppRecoveryOwner, 0, len(byID))
	for _, owner := range byID {
		ordered = append(ordered, owner)
	}
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i].InstanceID < ordered[j].InstanceID
	})
	for index, owner := range ordered {
		if !owner.RouteBearing {
			continue
		}
		if index > 0 {
			firstRoute := owner
			copy(ordered[1:index+1], ordered[0:index])
			ordered[0] = firstRoute
		}
		break
	}
	return ordered
}

func normalizedRecoveryAppIDs(instanceIDs []string) []string {
	seen := make(map[string]struct{}, len(instanceIDs))
	ids := make([]string, 0, len(instanceIDs))
	for _, rawID := range instanceIDs {
		instanceID := strings.TrimSpace(rawID)
		if instanceID == "" {
			continue
		}
		if _, exists := seen[instanceID]; exists {
			continue
		}
		seen[instanceID] = struct{}{}
		ids = append(ids, instanceID)
	}
	sort.Strings(ids)
	return ids
}

// PrepareTaskRecoveryApps suppresses every pending app before starting the
// ordinary background loop. Explicit app owners ignore this process-local
// suppression; the bulk loop does not, so it cannot bypass controller order or
// recurrence backoff.
func (s *GinServer) PrepareTaskRecoveryApps(instanceIDs []string) {
	if s == nil || s.appManager == nil || !s.LifecycleReady() {
		return
	}
	ids := normalizedRecoveryAppIDs(instanceIDs)
	s.taskRecoveryAppsMu.Lock()
	if s.taskRecoveryPendingApps == nil {
		s.taskRecoveryPendingApps = make(map[string]struct{}, len(ids))
	}
	for _, instanceID := range ids {
		s.taskRecoveryPendingApps[instanceID] = struct{}{}
		s.appManager.SuppressAutomaticRecovery(instanceID,
			"Automatic recovery is waiting for its serialized startup attempt")
	}
	s.taskRecoveryAppsMu.Unlock()

	s.decryptedOwnersStarted.Store(true)
	s.taskRecoveryAppSteadyOnce.Do(func() {
		// Restore skips the suppressed desired apps. It can still retire stale
		// non-desired runtime routes before the periodic loops start.
		s.appManager.RestoreServices(s.serverContext())
		s.refreshRemoteRuntime()
		s.appManager.StartBackgroundAfterInitial()
	})
}

// ReleaseTaskRecoveryApp releases only an app registered by Prepare. The cmd
// runner calls this only after the explicit attempt returned and its active
// marker progress was durably cleared.
func (s *GinServer) ReleaseTaskRecoveryApp(instanceID string) {
	if s == nil || s.appManager == nil {
		return
	}
	instanceID = strings.TrimSpace(instanceID)
	if instanceID == "" {
		return
	}
	s.taskRecoveryAppsMu.Lock()
	_, pending := s.taskRecoveryPendingApps[instanceID]
	if pending {
		delete(s.taskRecoveryPendingApps, instanceID)
	}
	s.taskRecoveryAppsMu.Unlock()
	if pending {
		s.appManager.ReleaseAutomaticRecoverySuppression(instanceID)
	}
}

func taskRecoveryAttemptContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, timeout)
}

func (s *GinServer) unlockTaskRecoveryOwner() TaskRecoveryOwner {
	return TaskRecoveryOwner{
		Name:    "unlock-chain",
		Timeout: taskRecoveryUnlockTimeout,
		Attempt: func(parent context.Context) (bool, error) {
			ctx, cancel := taskRecoveryAttemptContext(parent, taskRecoveryUnlockTimeout)
			defer cancel()
			if s.LifecycleReady() {
				s.startTaskRecoveryScheduler()
				return true, nil
			}
			if s.autounlockOrch == nil {
				return false, nil
			}
			result, err := s.autounlockOrch.Recover(ctx, func(chainCtx context.Context) error {
				_, chainErr := s.completeAutomaticUnlockChain(chainCtx)
				return chainErr
			})
			s.startTaskRecoveryScheduler()
			return result.Disposition == autounlock.RecoverDispositionUnlocked && s.LifecycleReady(), err
		},
	}
}

func (s *GinServer) startTaskRecoveryScheduler() {
	if s.autounlockScheduler == nil {
		return
	}
	s.taskRecoverySchedulerOnce.Do(func() {
		go s.autounlockScheduler.Run(s.serverContext())
	})
}

func (s *GinServer) keyslotTaskRecoveryOwner() TaskRecoveryOwner {
	return TaskRecoveryOwner{
		Name:    "keyslot",
		Timeout: taskRecoveryKeyslotTimeout,
		Attempt: func(ctx context.Context) (bool, error) {
			if ctx == nil {
				ctx = context.Background()
			}
			if err := ctx.Err(); err != nil {
				return false, err
			}
			s.keyslotReconciler.Start(s.serverContext())
			// Do not return merely because the operation deadline fired. The cmd
			// liveness owner must observe a still-running pass and arbitrate its
			// cancellation grace against process-fatal recovery.
			select {
			case <-s.keyslotReconciler.InitialPassDone():
			case <-s.serverContext().Done():
				return false, s.serverContext().Err()
			}
			if err := ctx.Err(); err != nil {
				return false, err
			}
			for slot, status := range s.keyslotReconciler.Status() {
				if status.LastError != "" {
					return false, fmt.Errorf("keyslot %d recovery: %s", slot, status.LastError)
				}
			}
			return true, nil
		},
	}
}

func (s *GinServer) storageTaskRecoveryOwner() TaskRecoveryOwner {
	return TaskRecoveryOwner{
		Name:    "storage",
		Timeout: taskRecoveryStorageTimeout,
		Attempt: func(ctx context.Context) (bool, error) {
			if ctx == nil {
				ctx = context.Background()
			}
			if err := ctx.Err(); err != nil {
				return false, err
			}
			bootCtx := pressure.WithWorkClass(ctx, pressure.WorkStorage)
			bootMode, err := storage.DetectBootMode(bootCtx, s.execRunner)
			if err != nil {
				return false, err
			}
			s.onboardingMgr.SetBootMode(bootMode)
			s.startOptionalStoragePreparation()
			if !s.storageMgr.IsPhase1Started() {
				return true, nil
			}
			// Phase 1 owns a server-scoped, internally bounded operation. Waiting
			// on that proof lets cmd detect a context-ignoring operation instead
			// of falsely returning while disk preparation is still mutating.
			if err := s.storageMgr.WaitForPhase1(s.serverContext()); err != nil {
				return false, err
			}
			if err := ctx.Err(); err != nil {
				return false, err
			}
			return true, nil
		},
	}
}

func (s *GinServer) catalogTaskRecoveryOwner() TaskRecoveryOwner {
	return TaskRecoveryOwner{
		Name:    "catalog",
		Timeout: taskRecoveryCatalogTimeout,
		Attempt: func(parent context.Context) (bool, error) {
			ctx, cancel := taskRecoveryAttemptContext(parent, taskRecoveryCatalogTimeout)
			defer cancel()
			if err := ctx.Err(); err != nil {
				return false, err
			}
			if err := s.catalogManager.EnsureCacheDir(); err != nil {
				return false, err
			}
			return true, ctx.Err()
		},
	}
}

func (s *GinServer) networkTaskRecoveryOwner() TaskRecoveryOwner {
	return TaskRecoveryOwner{
		Name:    "network",
		Timeout: taskRecoveryNetworkTimeout,
		Attempt: func(ctx context.Context) (bool, error) {
			if ctx == nil {
				ctx = context.Background()
			}
			if err := ctx.Err(); err != nil {
				return false, err
			}
			// The supervisor's first tick and steady loop currently share one
			// context. Use the server context so a successful joined tick does not
			// immediately cancel steady state; cmd's deadline/grace bridge remains
			// the liveness authority if StartRecovery fails to return.
			if err := s.networkSupervisor.StartRecovery(s.serverContext()); err != nil {
				return false, err
			}
			if err := ctx.Err(); err != nil {
				return false, err
			}
			return true, nil
		},
	}
}

func (s *GinServer) updateTaskRecoveryOwner() TaskRecoveryOwner {
	return TaskRecoveryOwner{
		Name:    "update",
		Timeout: taskRecoveryUpdateTimeout,
		Attempt: func(parent context.Context) (bool, error) {
			ctx, cancel := taskRecoveryAttemptContext(parent, taskRecoveryUpdateTimeout)
			defer cancel()
			watch := s.updateManager.Watch
			if recoveryManager, ok := s.updateManager.(recoveryUpdateManager); ok {
				recoveryManager.RunInitialRecovery(ctx)
				if err := ctx.Err(); err != nil {
					return false, err
				}
				watch = recoveryManager.WatchAfterInitial
			}
			s.taskRecoveryUpdateWatchOnce.Do(func() {
				go func() {
					if err := watch(s.serverContext()); err != nil && !errors.Is(err, context.Canceled) {
						log.Printf("WARN: update watchdog exited: %v", err)
					}
				}()
			})
			return true, nil
		},
	}
}
