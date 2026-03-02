package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"piccolod/internal/api"
	"piccolod/internal/container"
	"piccolod/internal/persistence"
	"piccolod/internal/services"
)

// installContainerGroup installs an app as a container group (network anchor + service containers).
// This is the unified install path for both service and workspace modes.
// For workspace mode, it prepares workspace disks and uses --rootfs mode.
// For service mode, it uses standard image-based containers.
func (m *AppManager) installContainerGroup(ctx context.Context, appDef *api.AppDefinition, instanceID string, layout appVolumeLayout, runtime container.PodmanRuntime, endpoints []services.ServiceEndpoint) (*AppInstance, error) {
	if m.serviceManager == nil {
		return nil, fmt.Errorf("app manager: service manager not configured")
	}
	if appDef == nil || appDef.Services == nil || len(appDef.Services) == 0 {
		return nil, fmt.Errorf("container group install requires services")
	}

	mode := piccoloModeFromExtensions(appDef.Extensions)
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

	// Validate and repair podman overlay storage before pulling images.
	// Previous failed installs (e.g., killed by HTTP timeout) can leave corrupted overlay
	// layers that cause subsequent pulls to fail with "readlink .../diff: no such file or directory".
	if repaired, err := m.containerManager.ValidateAndRepairStorage(ctx, runtime); err != nil {
		log.Printf("WARN: install %s: storage validation error: %v", instanceID, err)
	} else if repaired {
		log.Printf("INFO: install %s: repaired corrupted podman storage before pull", instanceID)
	}

	// Also validate the shared image runtime storage (shared imagestore across all app types).
	// All image pulls target the shared imagestore, so corruption there affects all modes.
	if imageRuntime, err := m.podmanImageRuntime(); err == nil {
		if repaired, vErr := m.containerManager.ValidateAndRepairStorage(ctx, imageRuntime); vErr != nil {
			log.Printf("WARN: install %s: image runtime storage validation error: %v", instanceID, vErr)
		} else if repaired {
			log.Printf("INFO: install %s: repaired image runtime storage before pull", instanceID)
		}
	}

	// Prepare storage for each service (pull images or init workspace disks)
	// Progress range 15-55% is divided equally among images
	const pullProgressMin = 15
	const pullProgressMax = 55
	numServices := len(appDef.Services)
	pullRangePerService := 0
	if numServices > 0 {
		pullRangePerService = (pullProgressMax - pullProgressMin) / numServices
	}

	// Block-native rootfs path: prepare per-service rootfs from golden LV snapshots.
	blockNativeRootfsMap := make(map[string]*rootfsMountInfo)
	useBlockNative := m.currentRootfsManager() != nil

	// Build IDMap config once (shared across all services — same per-app user).
	var idmap persistence.IDMapConfig
	if useBlockNative && runtime.Credential != nil {
		idmap = persistence.IDMapConfig{
			AppUID: runtime.Credential.Uid,
			AppGID: runtime.Credential.Gid,
		}
		username := container.AppUsername(instanceID)
		if subStart, subCount, lookupErr := container.LookupSubUIDRange(username); lookupErr == nil {
			idmap.SubUIDStart = subStart
			idmap.SubUIDCount = subCount
			idmap.SubGIDStart = subStart // same range for GID
			idmap.SubGIDCount = subCount
		} else {
			log.Printf("WARN: install %s: subuid lookup failed for %s: %v", instanceID, username, lookupErr)
		}
	}

	serviceIdx := 0
	for svcName := range appDef.Services {
		svc := appDef.Services[svcName]

		// Calculate progress range for this service
		progressRange := imagePullProgressRange{
			Min: pullProgressMin + (serviceIdx * pullRangePerService),
			Max: pullProgressMin + ((serviceIdx + 1) * pullRangePerService),
		}
		if serviceIdx == numServices-1 {
			progressRange.Max = pullProgressMax
		}

		if svc.Image != "" {
			// Pull image to shared imagestore (needed for golden LV flatten).
			callback := m.makeImagePullProgressCallback(ctx, instanceID, svcName, svc.Image, progressRange)
			if pullErr := m.pullToImagestore(ctx, svc.Image, callback); pullErr != nil {
				log.Printf("WARN: install %s: image pull failed for %s: %v", instanceID, svcName, pullErr)
			}

			if useBlockNative {
				// Get image digest.
				imageRuntime, err := m.podmanImageRuntime()
				if err != nil {
					return nil, fmt.Errorf("get image runtime: %w", err)
				}
				imgConfig, err := m.containerManager.InspectImage(ctx, imageRuntime, svc.Image)
				if err != nil {
					return nil, fmt.Errorf("inspect image %s: %w", svc.Image, err)
				}
				imageDigest := ""
				if len(imgConfig.RepoDigests) > 0 {
					imageDigest = imgConfig.RepoDigests[0]
				} else {
					imageDigest = imgConfig.Digest
				}

				rInfo, err := m.prepareRootfsStorage(ctx, mode, instanceID, svcName, imageDigest, svc.Image, idmap)
				if err != nil {
					return nil, fmt.Errorf("prepare rootfs for service '%s': %w", svcName, err)
				}
				blockNativeRootfsMap[svcName] = rInfo
			}
		}
		serviceIdx++
	}

	// Pull network anchor image directly into the per-app user's graphroot.
	// We do NOT use additionalimagestores because native overlay in rootless user
	// namespaces cannot access layers owned by a different user (UIDs unmapped).
	// The pause image is tiny (~500KB), so per-app duplication is negligible.
	if err := m.containerManager.PullImage(ctx, runtime, networkAnchorImage()); err != nil {
		return nil, fmt.Errorf("network anchor image pull failed: %w", err)
	}

	created := make([]string, 0, 1+len(appDef.Services))
	cleanup := func() {
		for i := len(created) - 1; i >= 0; i-- {
			cid := created[i]
			_ = m.containerManager.StopContainer(ctx, runtime, cid)
			_ = m.containerManager.RemoveContainer(ctx, runtime, cid)
		}
		// Cleanup rootfs on failure.
		if len(blockNativeRootfsMap) > 0 {
			m.detachAllServiceRootfs(ctx, instanceID, mode, appDef, nil)
		}
	}

	// 1) Create + start the network anchor (owns published ports + shared netns).
	anchorSpec := container.ContainerCreateSpec{
		Name:          networkAnchorContainerName(instanceID),
		Image:         networkAnchorImage(),
		PullPolicy:    "never", // Pre-pulled to per-app graphroot above.
		NetworkMode:   appNetworkMode(appDef),
		RestartPolicy: appRestartPolicy(appDef),
		Labels:        piccoloLabels(instanceID, networkAnchorServiceName, "network_anchor"),
		SecurityOpt:   selinuxDisableLabel(), // overlay context= ignored in user namespaces
	}
	for _, ep := range endpoints {
		anchorSpec.Ports = append(anchorSpec.Ports, container.PortMapping{Host: ep.HostBind, Container: ep.GuestPort})
	}
	// Add host gateway entries to the anchor (which owns the network namespace).
	// Service containers share this namespace and inherit the /etc/hosts entries.
	m.stateMu.RLock()
	oidcHost := m.oidcHostname
	m.stateMu.RUnlock()
	if entries, err := container.HostGatewayEntries(oidcHost); err == nil {
		anchorSpec.ExtraHosts = append(anchorSpec.ExtraHosts, entries...)
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
			_ = m.containerManager.StopContainer(ctx, runtime, zombieID)
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

		// Build container spec, with workspace info if available
		opts := serviceContainerOptions{
			layout:     layout,
			appDef:     appDef,
			instanceID: instanceID,
			primary:    primary,
			svcName:    svcName,
			anchorID:   anchorID,
			credential: runtime.Credential,
		}
		if svcRootfs, ok := blockNativeRootfsMap[svcName]; ok {
			opts.rootfsHandle = &svcRootfs.handle
			opts.goldenImgConfig = &svcRootfs.imgConfig
		}
		spec, err := m.buildServiceContainerSpec(opts)
		if err != nil {
			cleanup()
			return nil, err
		}
		// Per-app runtimes must never pull: images are pre-pulled to the shared
		// imagestore and per-app users lack write access to it.
		if spec.Image != "" {
			spec.PullPolicy = "never"
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
				_ = m.containerManager.StopContainer(ctx, runtime, zombieID)
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
			log.Printf("ERROR: install %s: start service container '%s' (cid=%s) failed: %v", instanceID, svcName, cid, err)
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
		Enabled:         true,
		PrimaryService:  primary,
		NetworkAnchorID: anchorID,
		Containers:      containers,
		CreatedAt:       now,
		UpdatedAt:       now,
		Definition:      appDef,
		CatalogSource:   CatalogSourceFromContext(ctx),
	}, nil
}
