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

	const createBaseProgress = 60
	const createSpanProgress = 25
	totalContainers := 1 + len(startOrder) // anchor + services
	doneContainers := 0

	subtasks := make([]map[string]any, 0, totalContainers)
	subtaskIdx := make(map[string]int, totalContainers)
	addSubtask := func(service, name, role string) {
		subtaskIdx[service] = len(subtasks)
		subtasks = append(subtasks, map[string]any{
			"service":  service,
			"name":     name,
			"role":     role,
			"progress": 0,
			"message":  "Pending",
		})
	}
	updateSubtask := func(service string, progress int, message string) {
		i, ok := subtaskIdx[service]
		if !ok {
			return
		}
		subtasks[i]["progress"] = progress
		subtasks[i]["message"] = message
	}
	cloneSubtasks := func(in []map[string]any) []map[string]any {
		out := make([]map[string]any, len(in))
		for i, task := range in {
			copied := make(map[string]any, len(task))
			for k, v := range task {
				copied[k] = v
			}
			out[i] = copied
		}
		return out
	}
	overallProgress := func(done int) int {
		if totalContainers <= 0 {
			return createBaseProgress
		}
		if done < 0 {
			done = 0
		}
		if done > totalContainers {
			done = totalContainers
		}
		return createBaseProgress + (createSpanProgress*done)/totalContainers
	}
	emitCreateProgress := func(message string) {
		m.emitProgressWithMetadata(
			ctx,
			taskTypeInstallApp,
			instanceID,
			taskPhaseCreatingContainer,
			overallProgress(doneContainers),
			message,
			false,
			map[string]any{"subtasks": cloneSubtasks(subtasks)},
			nil,
		)
	}

	addSubtask(networkAnchorServiceName, "network", "network_anchor")
	for _, svcName := range startOrder {
		addSubtask(svcName, svcName, "service")
	}
	emitCreateProgress(fmt.Sprintf("Creating containers (0/%d)", totalContainers))

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
	updateSubtask(networkAnchorServiceName, 10, "Creating")
	emitCreateProgress(fmt.Sprintf("Creating container (1/%d): network", totalContainers))
	for i := 0; i < 2; i++ {
		anchorID, err = m.containerManager.CreateContainer(ctx, runtime, anchorSpec)
		if err == nil {
			updateSubtask(networkAnchorServiceName, 50, "Created")
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

	updateSubtask(networkAnchorServiceName, 70, "Starting")
	if err := m.containerManager.StartContainer(ctx, runtime, anchorID); err != nil {
		updateSubtask(networkAnchorServiceName, 70, "Error")
		emitCreateProgress("Failed to start network container")
		cleanup()
		return nil, fmt.Errorf("failed to start network anchor: %w", err)
	}
	updateSubtask(networkAnchorServiceName, 100, "Running")
	doneContainers++
	emitCreateProgress(fmt.Sprintf("Created container (1/%d): network", totalContainers))

	// 2) Create + start all service containers attached to the anchor netns.
	containers := make(map[string]string, len(appDef.Services))
	for _, svcName := range startOrder {
		updateSubtask(svcName, 10, "Creating")
		emitCreateProgress(fmt.Sprintf("Creating container (%d/%d): %s", doneContainers+1, totalContainers, svcName))

		spec, err := m.buildServiceContainerSpec(layout, appDef, instanceID, primary, svcName, anchorID)
		if err != nil {
			cleanup()
			return nil, err
		}

		var cid string
		for i := 0; i < 2; i++ {
			cid, err = m.containerManager.CreateContainer(ctx, runtime, spec)
			if err == nil {
				updateSubtask(svcName, 50, "Created")
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
			updateSubtask(svcName, 50, "Error")
			emitCreateProgress(fmt.Sprintf("Failed to create container: %s", svcName))
			cleanup()
			return nil, fmt.Errorf("failed to create service container '%s': %w", svcName, err)
		}
		created = append(created, cid)

		updateSubtask(svcName, 70, "Starting")
		if err := m.containerManager.StartContainer(ctx, runtime, cid); err != nil {
			updateSubtask(svcName, 70, "Error")
			emitCreateProgress(fmt.Sprintf("Failed to start container: %s", svcName))
			cleanup()
			return nil, fmt.Errorf("failed to start service container '%s': %w", svcName, err)
		}

		updateSubtask(svcName, 100, "Running")
		doneContainers++
		emitCreateProgress(fmt.Sprintf("Created container (%d/%d): %s", doneContainers, totalContainers, svcName))

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
