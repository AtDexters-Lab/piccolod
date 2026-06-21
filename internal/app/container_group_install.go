package app

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
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
	rootfsMgr := m.currentRootfsManager()
	if rootfsMgr == nil {
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
			// Skip expensive image pull if a golden LV is already cached and fresh.
			if cachedDigest, _, ok := rootfsMgr.FindGoldenByImageRef(svc.Image); ok {
				useCache := true
				// Freshness check: verify the tag still resolves to the same digest.
				// If the registry is unreachable, fall back to the cached golden LV.
				if remoteDigest, err := resolveRemoteDigest(ctx, svc.Image); err == nil {
					if extractDigestHash(remoteDigest) != extractDigestHash(cachedDigest) {
						log.Printf("INFO: image %s tag moved (cached=%s, remote=%s) -- pulling fresh", svc.Image, cachedDigest, remoteDigest)
						useCache = false
					}
				} else {
					log.Printf("INFO: registry check for %s failed (%v) -- using cached golden LV", svc.Image, err)
				}
				if useCache {
					log.Printf("INFO: using cached golden LV for image %s (digest=%s) -- skipping pull", svc.Image, cachedDigest)
					m.emitProgress(ctx, taskTypeInstallApp, instanceID, taskPhasePullingImage, progressRange.Max,
						fmt.Sprintf("Using cached image %s", svc.Image), false, nil)
					rInfo, err := m.prepareRootfsStorage(ctx, mode, instanceID, svcName, cachedDigest, svc.Image, idmap, 0, "")
					if err != nil {
						return nil, fmt.Errorf("prepare rootfs for service '%s': %w", svcName, err)
					}
					blockNativeRootfsMap[svcName] = rInfo
					serviceIdx++
					continue
				}
			}

			// Pull to ephemeral runtime for digest, then prepare rootfs.
			// The same ephemeral runtime is reused by flattenFn inside EnsureGoldenLV,
			// avoiding a redundant second pull of the same image.
			ephRT, ephCleanup, ephErr := m.newFlattenRuntime(ctx)
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
			rootfsImageDigest := canonicalImageDigestKey(imageDigest)
			if rootfsImageDigest == "" {
				ephCleanup()
				return nil, fmt.Errorf("inspect image %s: canonical digest unavailable", svc.Image)
			}

			// Pass the pre-pulled runtime's root dir so flattenFn skips pulling again.
			// ephRT.Root is "<base>/root" — pass the parent (base) dir.
			prePulledDir := filepath.Dir(ephRT.Root)
			rInfo, err := m.prepareRootfsStorage(ctx, mode, instanceID, svcName, rootfsImageDigest, svc.Image, idmap, imgConfig.Size, prePulledDir)
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
		ephRT, ephCleanup, ephErr := m.newFlattenRuntime(ctx)
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
		anchorRootfsDigest := canonicalImageDigestKey(anchorDigest)
		if anchorRootfsDigest == "" {
			ephCleanup()
			return nil, fmt.Errorf("inspect anchor image: canonical digest unavailable")
		}
		anchorPrePulledDir := filepath.Dir(ephRT.Root)
		rInfo, err := m.prepareRootfsStorage(ctx, ModeService, instanceID, networkAnchorServiceName, anchorRootfsDigest, networkAnchorImage(), idmap, imgConfig.Size, anchorPrePulledDir)
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
			for svcName := range blockNativeRootfsMap {
				if prebuiltRootfs != nil {
					if _, isPrebuilt := prebuiltRootfs[svcName]; isPrebuilt {
						continue
					}
				}
				volID := persistence.ServiceRootfsVolumeID(instanceID, svcName)
				_ = rootfs.DetachRootfs(ctx, volID)
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
		pm := container.PortMapping{Host: ep.HostBind, Container: ep.GuestPort}
		if ep.Flow == api.FlowUDP {
			pm.Protocol = "udp"
		}
		anchorSpec.Ports = append(anchorSpec.Ports, pm)
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

	updateSubtask(networkAnchorServiceName, 10, "Creating")
	emitCreateProgress(fmt.Sprintf("Creating container (1/%d): network", totalContainers))
	anchorID, err := m.createContainerWithRetry(ctx, runtime, anchorSpec,
		fmt.Sprintf("install %s network-anchor", instanceID))
	if err != nil {
		return nil, fmt.Errorf("failed to create network anchor: %w", err)
	}
	updateSubtask(networkAnchorServiceName, 50, "Created")
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
	// Init scripts run inline after each container starts, before its dependents
	// are created. This ensures dependent services see fully initialized state.
	var initState *InitState
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

		cid, err := m.createContainerWithRetry(ctx, runtime, spec,
			fmt.Sprintf("install %s service=%s", instanceID, svcName))
		if err != nil {
			updateSubtask(svcName, 50, "Error")
			emitCreateProgress(fmt.Sprintf("Failed to create container: %s", svcName))
			cleanup()
			return nil, fmt.Errorf("failed to create service container '%s': %w", svcName, err)
		}
		updateSubtask(svcName, 50, "Created")
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

		// Run init script for this service BEFORE starting dependent services,
		// so dependents see fully initialized state (e.g., db schema/users).
		svc := appDef.Services[svcName]
		if svc.InitScript != nil && len(svc.InitScript.FileContent) > 0 {
			if initState == nil {
				initState = &InitState{Services: make(map[string]ServiceInitState)}
				m.emitProgress(ctx, taskTypeInstallApp, instanceID, taskPhaseRunningInit, 88,
					fmt.Sprintf("Running init script: %s", svcName), false, nil)
			}
			if err := m.runServiceInit(ctx, runtime, instanceID, svcName, cid, &svc, layout, initState); err != nil {
				cleanup()
				return nil, err
			}
		}
	}

	primaryCID := containers[primary]
	if primaryCID == "" {
		cleanup()
		return nil, fmt.Errorf("primary service container ID missing for '%s'", primary)
	}

	// Record container ID used for service proxy reconciliation (publishes live on the anchor).
	m.serviceManager.SetAppContainerID(instanceID, anchorID)

	var activeRootfs map[string]string
	if mode == ModeService {
		activeRootfs = make(map[string]string, len(blockNativeRootfsMap))
		for svcName, rInfo := range blockNativeRootfsMap {
			if rInfo == nil || strings.TrimSpace(rInfo.handle.VolumeID) == "" {
				continue
			}
			activeRootfs[svcName] = rInfo.handle.VolumeID
		}
		if len(activeRootfs) == 0 {
			activeRootfs = nil
		}
	}

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
		ActiveRootfs:    activeRootfs,
		Init:            initState,
	}, nil
}

// resolveRemoteDigest queries the registry for the current digest of an image
// reference without pulling layers. Uses skopeo inspect for a lightweight
// manifest-only fetch. Returns the digest (e.g., "sha256:abc123...").
func resolveRemoteDigest(ctx context.Context, imageRef string) (string, error) {
	cmd := exec.CommandContext(ctx, "skopeo", "inspect", "--format", "{{.Digest}}", "docker://"+imageRef)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("skopeo inspect %s: %w", imageRef, err)
	}
	digest := strings.TrimSpace(string(out))
	if digest == "" {
		return "", fmt.Errorf("skopeo returned empty digest for %s", imageRef)
	}
	return digest, nil
}

func (m *AppManager) resolveRemoteImageDigest(ctx context.Context, imageRef string) (string, error) {
	if m != nil && m.imageDigestResolver != nil {
		return m.imageDigestResolver(ctx, imageRef)
	}
	digest, err := resolveRemoteDigest(ctx, imageRef)
	if err == nil {
		return digest, nil
	}
	pulledDigest, pullErr := m.resolveImageDigestByPull(ctx, imageRef)
	if pullErr != nil {
		return "", fmt.Errorf("%w; fallback pull/inspect: %v", err, pullErr)
	}
	return pulledDigest, nil
}

func (m *AppManager) resolveImageDigestByPull(ctx context.Context, imageRef string) (string, error) {
	if m == nil || m.containerManager == nil {
		return "", fmt.Errorf("container manager not configured")
	}
	ephRT, ephCleanup, err := m.newFlattenRuntime(ctx)
	if err != nil {
		return "", fmt.Errorf("create ephemeral runtime: %w", err)
	}
	defer ephCleanup()
	if err := m.containerManager.PullImage(ctx, ephRT, imageRef); err != nil {
		return "", fmt.Errorf("pull image %s: %w", imageRef, err)
	}
	imgConfig, err := m.containerManager.InspectImage(ctx, ephRT, imageRef)
	if err != nil {
		return "", fmt.Errorf("inspect image %s: %w", imageRef, err)
	}
	digest := imageConfigDigest(imgConfig)
	if digest == "" {
		return "", fmt.Errorf("inspect image %s: digest unavailable", imageRef)
	}
	return digest, nil
}

// runServiceInit executes a single service's init script, updating initState
// with the result. Returns an error if the init fails (caller should cleanup
// and abort the install).
func (m *AppManager) runServiceInit(ctx context.Context, runtime container.PodmanRuntime, instanceID, svcName, cid string, svc *api.AppService, layout appVolumeLayout, initState *InitState) error {
	startedAt := time.Now()
	initState.Services[svcName] = ServiceInitState{
		Status:    InitStatusRunning,
		StartedAt: startedAt,
	}

	readyTimeout := 30 * time.Second
	if svc.InitScript.ReadyTimeout != "" {
		if d, err := time.ParseDuration(svc.InitScript.ReadyTimeout); err == nil {
			readyTimeout = d
		}
	}
	if err := m.waitForContainerReady(ctx, runtime, cid, readyTimeout); err != nil {
		initState.Services[svcName] = ServiceInitState{
			Status: InitStatusFailed, StartedAt: startedAt, Error: fmt.Sprintf("container not ready: %v", err),
		}
		log.Printf("ERROR: init script for service '%s' (%s): container not ready: %v", svcName, instanceID, err)
		return fmt.Errorf("init: service '%s' container not ready: %w", svcName, err)
	}

	execTimeout := 120 * time.Second
	if svc.InitScript.Timeout != "" {
		if d, err := time.ParseDuration(svc.InitScript.Timeout); err == nil {
			execTimeout = d
		}
	}

	log.Printf("INFO: init script for service '%s' (%s) started: shell=%s file=%s timeout=%s",
		svcName, instanceID, svc.InitScript.Shell, svc.InitScript.File, execTimeout)

	exitCode, output, execErr := m.containerManager.ExecScript(ctx, runtime, cid,
		container.ExecScriptOptions{
			Shell:      svc.InitScript.Shell,
			ScriptPath: path.Join("/piccolo/config", svc.InitScript.File),
			Env:        svc.InitScript.Env,
			Timeout:    execTimeout,
		})

	// Write init log to the control-plane metadata dir, not the per-app
	// data volume. The metadata dir survives cleanupInstallResources() on
	// install failure, so the log remains available for debugging.
	// Mode 0600: init output may contain secrets injected via env vars.
	if state, stateErr := m.ensureStateManager(); stateErr == nil {
		metaDir := state.AppMetaDir(instanceID)
		if err := os.MkdirAll(metaDir, 0700); err != nil {
			log.Printf("WARN: failed to create meta dir for init log (%s): %v", instanceID, err)
		} else {
			logPath := filepath.Join(metaDir, fmt.Sprintf("init-%s.log", svcName))
			if err := os.WriteFile(logPath, []byte(output), 0600); err != nil {
				log.Printf("WARN: failed to write init log for service '%s' (%s): %v", svcName, instanceID, err)
			}
		}
	}

	if execErr != nil {
		initState.Services[svcName] = ServiceInitState{
			Status:      InitStatusFailed,
			StartedAt:   startedAt,
			CompletedAt: time.Now(),
			ExitCode:    exitCode,
			Error:       execErr.Error(),
		}
		log.Printf("ERROR: init script for service '%s' (%s) failed (exit=%d): %v", svcName, instanceID, exitCode, execErr)
		return fmt.Errorf("init script for service '%s' failed: %w", svcName, execErr)
	}

	initState.Services[svcName] = ServiceInitState{
		Status:      InitStatusCompleted,
		StartedAt:   startedAt,
		CompletedAt: time.Now(),
	}
	log.Printf("INFO: init script for service '%s' (%s) completed in %s",
		svcName, instanceID, time.Since(startedAt).Round(time.Millisecond))
	return nil
}

// waitForContainerReady polls the container state until it is running.
// The init script itself is responsible for app-level readiness checks.
func (m *AppManager) waitForContainerReady(ctx context.Context, runtime container.PodmanRuntime, containerID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	// Check at least once even if timeout is 0s.
	for {
		state, err := m.containerManager.InspectContainerState(ctx, runtime, containerID)
		if err != nil {
			return err
		}
		if !state.Exists {
			return fmt.Errorf("container no longer exists")
		}
		if state.Running {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("container not running after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}
}

// extractDigestHash extracts the "sha256:..." portion from a digest string.
// Handles both bare digests ("sha256:abc...") and repo-qualified digests
// ("docker.io/library/nginx@sha256:abc...").
func extractDigestHash(digest string) string {
	if i := strings.LastIndex(digest, "sha256:"); i >= 0 {
		return digest[i:]
	}
	return digest
}
