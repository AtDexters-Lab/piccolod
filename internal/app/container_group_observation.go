package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"piccolod/internal/api"
	"piccolod/internal/container"
)

// containerGroupObservationOutcome separates authoritative runtime facts from
// incomplete observations. Unknown never authorizes a container, route, or
// persisted-metadata effect.
type containerGroupObservationOutcome uint8

const (
	containerGroupUnknown containerGroupObservationOutcome = iota
	containerGroupRunning
	containerGroupStopped
	containerGroupMissing
	containerGroupStale
)

func (o containerGroupObservationOutcome) String() string {
	switch o {
	case containerGroupRunning:
		return "running"
	case containerGroupStopped:
		return "stopped"
	case containerGroupMissing:
		return "missing"
	case containerGroupStale:
		return "stale"
	default:
		return "unknown"
	}
}

type observedContainerState struct {
	ID    string
	State container.ContainerState
}

type containerGroupObservation struct {
	Outcome  containerGroupObservationOutcome
	Anchor   observedContainerState
	Services map[string]observedContainerState
	Owned    []container.ContainerListItem
	Err      error
}

func (o containerGroupObservation) known() bool {
	return o.Outcome != containerGroupUnknown && o.Err == nil
}

func unknownContainerGroupObservation(err error) containerGroupObservation {
	if err == nil {
		err = errors.New("container group observation incomplete")
	}
	return containerGroupObservation{Outcome: containerGroupUnknown, Err: err}
}

// observeContainerRef observes a recorded container and, when the recorded ID
// is authoritatively absent, resolves the deterministic name. A typed
// ContainerNotFoundError is known absence; every other failure is unknown.
func (m *AppManager) observeContainerRef(ctx context.Context, runtime container.PodmanRuntime, recordedID, name string) (observedContainerState, error) {
	recordedID = strings.TrimSpace(recordedID)
	if recordedID != "" {
		state, err := m.containerManager.InspectContainerState(ctx, runtime, recordedID)
		if err != nil {
			return observedContainerState{}, fmt.Errorf("inspect recorded container %s: %w", recordedID, err)
		}
		if state.Exists {
			return observedContainerState{ID: recordedID, State: state}, nil
		}
	}

	resolvedID, err := m.containerManager.ResolveContainerIDByName(ctx, runtime, name)
	if err != nil {
		var notFound *container.ContainerNotFoundError
		if errors.As(err, &notFound) {
			return observedContainerState{State: container.ContainerState{Exists: false}}, nil
		}
		return observedContainerState{}, fmt.Errorf("resolve container %s: %w", name, err)
	}
	resolvedID = strings.TrimSpace(resolvedID)
	if resolvedID == "" {
		return observedContainerState{}, fmt.Errorf("resolve container %s: %w", name, &container.InvalidOutputError{Operation: "resolve container id"})
	}
	state, err := m.containerManager.InspectContainerState(ctx, runtime, resolvedID)
	if err != nil {
		return observedContainerState{}, fmt.Errorf("inspect resolved container %s (%s): %w", name, resolvedID, err)
	}
	if !state.Exists {
		// The name disappeared between two successful Podman observations. That
		// is still authoritative absence, not an execution failure.
		return observedContainerState{State: state}, nil
	}
	return observedContainerState{ID: resolvedID, State: state}, nil
}

// observeContainerGroup produces one complete, immutable snapshot for a
// reconciliation decision. Enumeration is included so a failed `podman ps`
// cannot be followed by stale-container pruning or group recreation.
func (m *AppManager) observeContainerGroup(ctx context.Context, runtime container.PodmanRuntime, appInst *AppInstance, def *api.AppDefinition) containerGroupObservation {
	if m == nil || m.containerManager == nil || appInst == nil || def == nil || def.Services == nil {
		return unknownContainerGroupObservation(errors.New("invalid container group observation input"))
	}

	owned, err := m.containerManager.ListContainersByLabel(ctx, runtime, "io.piccolo.instance", appInst.InstanceID)
	if err != nil {
		return unknownContainerGroupObservation(fmt.Errorf("list app containers: %w", err))
	}

	anchor, err := m.observeContainerRef(ctx, runtime, appInst.NetworkAnchorID, networkAnchorContainerName(appInst.InstanceID))
	if err != nil {
		return unknownContainerGroupObservation(fmt.Errorf("observe network anchor: %w", err))
	}

	primary := primaryServiceFor(def, appInst)
	order, err := serviceStartOrder(def.Services)
	if err != nil {
		return unknownContainerGroupObservation(fmt.Errorf("resolve service order: %w", err))
	}
	services := make(map[string]observedContainerState, len(order))
	for _, serviceName := range order {
		observed, observeErr := m.observeContainerRef(
			ctx,
			runtime,
			appInst.Containers[serviceName],
			containerNameForService(appInst.InstanceID, serviceName, primary),
		)
		if observeErr != nil {
			return unknownContainerGroupObservation(fmt.Errorf("observe service %s: %w", serviceName, observeErr))
		}
		services[serviceName] = observed
	}

	outcome := containerGroupRunning
	all := make([]observedContainerState, 0, 1+len(services))
	all = append(all, anchor)
	for _, serviceName := range order {
		all = append(all, services[serviceName])
	}
	for _, observed := range all {
		if observed.State.Stale {
			outcome = containerGroupStale
			break
		}
		if !observed.State.Exists {
			outcome = containerGroupMissing
			continue
		}
		if !observed.State.Running && outcome == containerGroupRunning {
			outcome = containerGroupStopped
		}
	}

	return containerGroupObservation{
		Outcome:  outcome,
		Anchor:   anchor,
		Services: services,
		Owned:    owned,
	}
}

// applyContainerGroupObservation persists only IDs proven by a complete
// observation. It never clears an old ID merely because absence was observed;
// the owning lifecycle effect persists that mutation at its normal boundary.
func (m *AppManager) applyContainerGroupObservation(state *FilesystemStateManager, appInst *AppInstance, observed containerGroupObservation) error {
	if !observed.known() {
		return fmt.Errorf("cannot apply %s container group observation: %w", observed.Outcome, observed.Err)
	}
	changed := false
	if observed.Anchor.ID != "" && observed.Anchor.ID != strings.TrimSpace(appInst.NetworkAnchorID) {
		appInst.NetworkAnchorID = observed.Anchor.ID
		changed = true
	}
	for serviceName, service := range observed.Services {
		if service.ID == "" || service.ID == strings.TrimSpace(appInst.Containers[serviceName]) {
			continue
		}
		if appInst.Containers == nil {
			appInst.Containers = make(map[string]string)
		}
		appInst.Containers[serviceName] = service.ID
		changed = true
	}
	if !changed {
		return nil
	}
	if err := state.StoreAppMetadata(appInst); err != nil {
		return fmt.Errorf("persist observed container IDs: %w", err)
	}
	return nil
}
