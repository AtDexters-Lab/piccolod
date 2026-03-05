package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"time"

	"piccolod/internal/api"
	"piccolod/internal/container"
	"piccolod/internal/persistence"
	"piccolod/internal/services"
)

// installContainerGroup installs an app as a container group (network anchor + service containers).
// All containers use --rootfs from golden LV snapshots (block-native architecture).
// For workspace mode, workspace disks are prepared via golden LVs.
// When prebuiltRootfs is non-nil, services with entries skip image pull + rootfs creation (used by clone).
func (m *AppManager) installContainerGroup(ctx context.Context, appDef *api.AppDefinition, instanceID string, layout appVolumeLayout, runtime container.PodmanRuntime, endpoints []services.ServiceEndpoint, prebuiltRootfs map[string]*rootfsMountInfo) (*AppInstance, error) {
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

	// Prepare storage for each service (pull images or init workspace disks)
	// Progress range 15-55% is divided equally among images
	const pullProgressMin = 15
	const pullProgressMax = 55
	numServices := len(appDef.Services)
	pullRangePerService := 0
	if numServices > 0 {
		pullRangePerService = (pullProgressMax - pullProgressMin) / numServices
	}

	// Block-native rootfs: prepare per-service rootfs from golden LV snapshots.
	if m.currentRootfsManager() == nil {
		return nil, fmt.Errorf("rootfs volume manager not configured")
	}
	blockNativeRootfsMap := make(map[string]*rootfsMountInfo)

	// Build IDMap config once (shared across all services — same per-app user).
	var idmap persistence.IDMapConfig
	if runtime.Credential != nil {
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
		// Skip storage prep for services with prebuilt rootfs (clone path).
		if prebuiltRootfs != nil {
			if rInfo, ok := prebuiltRootfs[svcName]; ok {
				blockNativeRootfsMap[svcName] = rInfo
				serviceIdx++
				continue
			}
		}

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
			// Pull to ephemeral runtime for digest, then prepare rootfs.
			// The same ephemeral runtime is reused by flattenFn inside EnsureGoldenLV,
			// avoiding a redundant second pull of the same image.
			ephRT, ephCleanup, ephErr := newEphemeralFlattenRuntime(m.runtimeUser)
			if ephErr != nil {
				return nil, fmt.Errorf("create ephemeral runtime: %w", ephErr)
			}
			callback := m.makeImagePullProgressCallback(ctx, instanceID, svcName, svc.Image, progressRange)
			if pullErr := m.containerManager.PullImageWithProgress(ctx, ephRT, svc.Image, callback); pullErr != nil {
				ephCleanup()
				return nil, fmt.Errorf("pull image %s: %w", svc.Image, pullErr)
			}
			imgConfig, inspErr := m.containerManager.InspectImage(ctx, ephRT, svc.Image)
			if inspErr != nil {
				ephCleanup()
				return nil, fmt.Errorf("inspect image %s: %w", svc.Image, inspErr)
			}
			imageDigest := ""
			if len(imgConfig.RepoDigests) > 0 {
				imageDigest = imgConfig.RepoDigests[0]
			} else {
				imageDigest = imgConfig.Digest
			}

			// Pass the pre-pulled runtime's root dir so flattenFn skips pulling again.
			// ephRT.Root is "<base>/root" — pass the parent (base) dir.
			prePulledDir := filepath.Dir(ephRT.Root)
			rInfo, err := m.prepareRootfsStorage(ctx, mode, instanceID, svcName, imageDigest, svc.Image, idmap, imgConfig.Size, prePulledDir)
			ephCleanup()
			if err != nil {
				return nil, fmt.Errorf("prepare rootfs for service '%s': %w", svcName, err)
			}
			blockNativeRootfsMap[svcName] = rInfo
		}
		serviceIdx++
	}

	// Prepare network anchor rootfs via golden LV pipeline.
	// Anchor always uses ModeService regardless of app mode — it's a service container.
	// Skip if the caller already provides an attached anchor rootfs (e.g., image update path).
	if prebuiltRootfs != nil {
		if rInfo, ok := prebuiltRootfs[networkAnchorServiceName]; ok {
			blockNativeRootfsMap[networkAnchorServiceName] = rInfo
		}
	}
	if _, ok := blockNativeRootfsMap[networkAnchorServiceName]; !ok {
		ephRT, ephCleanup, ephErr := newEphemeralFlattenRuntime(m.runtimeUser)
		if ephErr != nil {
			return nil, fmt.Errorf("create ephemeral runtime for anchor: %w", ephErr)
		}
		if pullErr := m.containerManager.PullImage(ctx, ephRT, networkAnchorImage()); pullErr != nil {
			ephCleanup()
			return nil, fmt.Errorf("pull anchor image: %w", pullErr)
		}
		imgConfig, inspErr := m.containerManager.InspectImage(ctx, ephRT, networkAnchorImage())
		if inspErr != nil {
			ephCleanup()
			return nil, fmt.Errorf("inspect anchor image: %w", inspErr)
		}
		anchorDigest := ""
		if len(imgConfig.RepoDigests) > 0 {
			anchorDigest = imgConfig.RepoDigests[0]
		} else {
			anchorDigest = imgConfig.Digest
		}
		anchorPrePulledDir := filepath.Dir(ephRT.Root)
		rInfo, err := m.prepareRootfsStorage(ctx, ModeService, instanceID, networkAnchorServiceName, anchorDigest, networkAnchorImage(), idmap, imgConfig.Size, anchorPrePulledDir)
		ephCleanup()
		if err != nil {
			return nil, fmt.Errorf("prepare rootfs for network anchor: %w", err)
		}
		blockNativeRootfsMap[networkAnchorServiceName] = rInfo
	}

	created := make([]string, 0, 1+len(appDef.Services))
	cleanup := func() {
		for i := len(created) - 1; i >= 0; i-- {
			cid := created[i]
			_ = m.containerManager.StopContainer(ctx, runtime, cid)
			_ = m.containerManager.RemoveContainer(ctx, runtime, cid)
		}
		// Detach only locally-created rootfs volumes, not prebuilt ones
		// (the caller owns those handles and is responsible for their lifecycle).
		if rootfs := m.currentRootfsManager(); rootfs != nil {
			for svcName, rInfo := range blockNativeRootfsMap {
				if prebuiltRootfs != nil {
					if _, isPrebuilt := prebuiltRootfs[svcName]; isPrebuilt {
						continue
					}
				}
				volID := persistence.ServiceRootfsVolumeID(instanceID, svcName)
				_ = rootfs.DetachRootfs(ctx, volID)
				_ = rInfo // suppress unused warning
			}
		}
	}

	// 1) Create + start the network anchor (owns published ports + shared netns).
	anchorSpec := container.ContainerCreateSpec{
		Name:          networkAnchorContainerName(instanceID),
		NetworkMode:   appNetworkMode(appDef),
		RestartPolicy: appRestartPolicy(appDef),
		Labels:        piccoloLabels(instanceID, networkAnchorServiceName, "network_anchor"),
		SecurityOpt:   selinuxDisableLabel(), // overlay context= ignored in user namespaces
	}
	anchorRootfs := blockNativeRootfsMap[networkAnchorServiceName]
	anchorSpec.Rootfs = anchorRootfs.handle.MountPath
	anchorSpec.RootfsOverlay = anchorRootfs.handle.ReadOnly
	// Don't set ReadOnly — the :O overlay upper layer must be writable.
	// The underlying btrfs mount is already read-only; writes go to the ephemeral overlay.
	// In --rootfs mode, Podman doesn't read image config, so we must supply
	// entrypoint/cmd from the golden image metadata explicitly.
	anchorSpec.Entrypoint = anchorRootfs.imgConfig.Entrypoint
	anchorSpec.Command = anchorRootfs.imgConfig.Cmd
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
		// Per-app runtimes must never pull: service containers use --rootfs
		// from golden LV snapshots.
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
