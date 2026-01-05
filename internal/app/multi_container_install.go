package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"piccolod/internal/api"
	"piccolod/internal/container"
	"piccolod/internal/services"
)

func (m *AppManager) installMultiContainer(ctx context.Context, appDef *api.AppDefinition, instanceID, displayName string, layout appVolumeLayout, runtime container.PodmanRuntime, endpoints []services.ServiceEndpoint) (*AppInstance, error) {
	if m.serviceManager == nil {
		return nil, fmt.Errorf("app manager: service manager not configured")
	}
	if appDef == nil || appDef.Services == nil || len(appDef.Services) == 0 {
		return nil, fmt.Errorf("multi-container install requires services")
	}

	primary := primaryServiceFor(appDef, nil)
	startOrder, err := serviceStartOrder(appDef.Services)
	if err != nil {
		return nil, err
	}

	// Defensive cleanup: prune labeled containers that belong to this instance but are not expected.
	// This addresses partial installs (e.g., crash mid-way) where no app state exists yet to trigger reconcile.
	expectedNames := make(map[string]struct{}, 1+len(appDef.Services))
	expectedNames[networkAnchorContainerName(instanceID)] = struct{}{}
	for svcName := range appDef.Services {
		expectedNames[containerNameForService(instanceID, svcName, primary)] = struct{}{}
	}
	m.pruneMultiContainerZombies(ctx, runtime, instanceID, expectedNames)

	// Best-effort pulls: multi-container apps may reference multiple images.
	images := map[string]struct{}{}
	images[networkAnchorImage()] = struct{}{}
	for _, svc := range appDef.Services {
		if svc.Image != "" {
			images[svc.Image] = struct{}{}
		}
	}
	for img := range images {
		if err := m.containerManager.PullImage(ctx, runtime, img); err != nil {
			log.Printf("WARN: install %s: image pull failed image=%s: %v", instanceID, img, err)
		}
	}

	created := make([]string, 0, 1+len(appDef.Services))
	cleanup := func() {
		for i := len(created) - 1; i >= 0; i-- {
			cid := created[i]
			_ = m.containerManager.StopContainer(ctx, runtime, cid)
			_ = m.containerManager.RemoveContainer(ctx, runtime, cid)
		}
	}

	// 1) Create + start the network anchor (owns published ports + shared netns).
	anchorSpec := container.ContainerCreateSpec{
		Name:          networkAnchorContainerName(instanceID),
		Image:         networkAnchorImage(),
		NetworkMode:   appNetworkMode(appDef),
		RestartPolicy: appRestartPolicy(appDef),
		Labels:        piccoloLabels(instanceID, networkAnchorServiceName, "network_anchor"),
	}
	for _, ep := range endpoints {
		anchorSpec.Ports = append(anchorSpec.Ports, container.PortMapping{Host: ep.HostBind, Container: ep.GuestPort})
	}
	if err := container.ValidateContainerSpec(anchorSpec); err != nil {
		return nil, fmt.Errorf("invalid network anchor spec: %w", err)
	}

	var anchorID string
	for i := 0; i < 2; i++ {
		anchorID, err = m.containerManager.CreateContainer(ctx, runtime, anchorSpec)
		if err == nil {
			break
		}

		// If PortInUse, let the caller retry allocation.
		var portErr *container.PortInUseError
		if errors.As(err, &portErr) {
			break
		}

		// Cleanup zombies by deterministic name.
		zombieID := ""
		var nameErr *container.NameInUseError
		if errors.As(err, &nameErr) {
			zombieID = nameErr.ID
		} else if id, resolveErr := m.containerManager.ResolveContainerIDByName(ctx, runtime, anchorSpec.Name); resolveErr == nil {
			zombieID = id
		}
		if zombieID != "" {
			log.Printf("INFO: install %s: removing zombie container %s (network anchor)", instanceID, zombieID)
			_ = m.containerManager.RemoveContainer(ctx, runtime, zombieID)
			continue
		}
		break
	}
	if err != nil {
		return nil, fmt.Errorf("failed to create network anchor: %w", err)
	}
	created = append(created, anchorID)

	if err := m.containerManager.StartContainer(ctx, runtime, anchorID); err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to start network anchor: %w", err)
	}

	// 2) Create + start all service containers attached to the anchor netns.
	containers := make(map[string]string, len(appDef.Services))
	for _, svcName := range startOrder {
		spec, err := m.buildServiceContainerSpec(layout, appDef, instanceID, primary, svcName, anchorID)
		if err != nil {
			cleanup()
			return nil, err
		}

		var cid string
		for i := 0; i < 2; i++ {
			cid, err = m.containerManager.CreateContainer(ctx, runtime, spec)
			if err == nil {
				break
			}

			// PortInUse should not happen for service containers (no publishes).
			var portErr *container.PortInUseError
			if errors.As(err, &portErr) {
				break
			}

			zombieID := ""
			var nameErr *container.NameInUseError
			if errors.As(err, &nameErr) {
				zombieID = nameErr.ID
			} else if id, resolveErr := m.containerManager.ResolveContainerIDByName(ctx, runtime, spec.Name); resolveErr == nil {
				zombieID = id
			}
			if zombieID != "" {
				log.Printf("INFO: install %s: removing zombie container %s (service=%s)", instanceID, zombieID, svcName)
				_ = m.containerManager.RemoveContainer(ctx, runtime, zombieID)
				continue
			}
			break
		}
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("failed to create service container '%s': %w", svcName, err)
		}
		created = append(created, cid)

		if err := m.containerManager.StartContainer(ctx, runtime, cid); err != nil {
			cleanup()
			return nil, fmt.Errorf("failed to start service container '%s': %w", svcName, err)
		}

		containers[svcName] = cid
	}

	primaryCID := containers[primary]
	if primaryCID == "" {
		cleanup()
		return nil, fmt.Errorf("primary service container ID missing for '%s'", primary)
	}

	// Record container ID used for service proxy reconciliation (publishes live on the anchor).
	m.serviceManager.SetAppContainerID(instanceID, anchorID)

	now := time.Now()
	return &AppInstance{
		InstanceID:      instanceID,
		DisplayName:     displayName,
		Status:          "running",
		ContainerID:     primaryCID,
		PrimaryService:  primary,
		NetworkAnchorID: anchorID,
		Containers:      containers,
		CreatedAt:       now,
		UpdatedAt:       now,
		Definition:      appDef,
	}, nil
}
