package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"piccolod/internal/api"
	"piccolod/internal/cluster"
	"piccolod/internal/container"
	"piccolod/internal/events"
	"piccolod/internal/persistence"
	"piccolod/internal/router"
	"piccolod/internal/services"
	"piccolod/internal/state/paths"
)

// AppManager manages application lifecycle with filesystem-based state storage
type AppManager struct {
	containerManager ContainerManager
	stateManager     *FilesystemStateManager
	stateBaseDir     string
	stateInitMu      sync.Mutex
	serviceManager   *services.ServiceManager
	routeRegistrar   router.Registrar
	progressReporter events.ProgressReporter
	eventBus         *events.Bus
	eventsMu         sync.Mutex
	eventCancel      context.CancelFunc
	eventSubCancels  []func()
	eventsWG         sync.WaitGroup
	reconcileMu      sync.Mutex
	reconcileCancel  context.CancelFunc
	reconcileWG      sync.WaitGroup
	stateMu          sync.RWMutex
	leadershipMu     sync.RWMutex
	leadershipState  map[string]cluster.Role
	lockReader       LockStateReader
	volumeManager    persistence.VolumeManager
	restoreMu        sync.Mutex
	pendingRestore   bool
	lockOverrideMu   sync.RWMutex
	lockOverride     *bool
	mountVerifier    func(string) error

	// In-memory observed status: derived from container state during reconciliation.
	// Published via event bus and returned in API responses. Never persisted.
	observedStatus        map[string]string
	observedStatusMessage map[string]string // transient status message for UI context
	observedStatusMu      sync.RWMutex

	// Internal CA path for OIDC trust
	internalCAPath string

	// OIDC hostname for container --add-host entries (machine-specific, e.g. "piccolo-abc123.local")
	oidcHostname string

	// runtimeUser holds the resolved rootless runtime user credentials.
	// Required unconditionally — per-app isolation is a security invariant.
	runtimeUser container.RuntimeUser

	// credentialResolver overrides per-app user provisioning for tests.
	// When non-nil, resolveAppCredential returns this instead of calling ProvisionAppUser.
	credentialResolver func(instanceID string) (*syscall.Credential, string, error)

	// Per-app user orphan cleanup runs once at first reconciliation.
	orphanCleanupOnce sync.Once

	// Block-native rootfs volume manager for golden LVs and workspace/service rootfs.
	rootfsMgr persistence.RootfsVolumeManager

	// Shared scratch ephemeral thin LV for flatten operations.
	scratchMu       sync.Mutex
	scratchHandle   persistence.VolumeHandle
	scratchAttached bool

	// Catalog manifest sync host. Wired by GinServer post-construction. nil
	// for tests that do not exercise sync. Guarded by stateMu.
	syncHost SyncHost

	// Tracks in-flight sync operations per instance ID. Used to reject
	// concurrent /sync/trigger calls (AR-2). Guarded by syncStateMu.
	syncInFlight map[string]bool
	syncStateMu  sync.Mutex
}

var (
	ErrLocked            = errors.New("app manager: persistence locked")
	ErrNotLeader         = errors.New("app manager: not leader")
	ErrVolumeUnavailable = errors.New("app manager: persistence volume not mounted")
	ErrAppUninstalling   = errors.New("app manager: app is being uninstalled")
)

// LockStateReader exposes the control lock state.
type LockStateReader interface {
	ControlLocked() bool
}

const (
	maxInstallPortRetries = 5

	// scratchFlattenVolumeID is the ephemeral thin LV used as backing store
	// for flatten operations (image pull → export → golden LV creation).
	scratchFlattenVolumeID = "scratch-flatten"

	// cleanupBudget is the timeout for detached cleanup contexts used when the
	// caller's context may have expired (e.g. install timeout). Must fit within
	// the server's shutdown drain window to complete before systemd SIGKILL.
	cleanupBudget = 60 * time.Second
)

// parseEnvSlice converts OCI-style env slice (KEY=VALUE) to a map.
// If duplicate keys exist, the last value wins.
func parseEnvSlice(envSlice []string) map[string]string {
	result := make(map[string]string, len(envSlice))
	for _, entry := range envSlice {
		if idx := strings.Index(entry, "="); idx > 0 {
			key := entry[:idx]
			value := entry[idx+1:]
			result[key] = value
		}
	}
	return result
}

// mergeEnvMaps merges base and override env maps.
// Values from override take precedence over base.
func mergeEnvMaps(base, override map[string]string) map[string]string {
	result := make(map[string]string, len(base)+len(override))
	for k, v := range base {
		result[k] = v
	}
	for k, v := range override {
		result[k] = v
	}
	return result
}

// NewAppManagerWithServices creates a new filesystem-based app manager with an injected ServiceManager
func NewAppManagerWithServices(containerManager ContainerManager, stateDir string, serviceManager *services.ServiceManager, lockReader LockStateReader) (*AppManager, error) {
	base := stateDir
	if strings.TrimSpace(base) == "" {
		base = paths.CoreRoot()
	}
	base = filepath.Clean(base)

	// Resolve rootless runtime user. All Podman commands run as this user.
	// Per-app isolation is unconditional — the daemon refuses to start without this user.
	runtimeUser, err := container.ResolveRuntimeCredential(container.RuntimeUsername)
	if err != nil {
		return nil, fmt.Errorf("app manager: %w", err)
	}

	if err := container.EnsureXDGRuntimeDir(runtimeUser.Credential.Uid, runtimeUser.Credential.Gid); err != nil {
		return nil, fmt.Errorf("app manager: failed to create XDG_RUNTIME_DIR for rootless Podman: %w", err)
	}
	container.CheckCgroupDelegation(runtimeUser.Credential.Uid)

	// Ensure cgroup v2 controllers are delegated to user sessions.
	// Must run before ProvisionAppUser so new user@.service instances
	// start with memory/cpu/pids/io controllers available.
	if err := container.EnsureCgroupDelegation(); err != nil {
		log.Printf("WARN: failed to ensure cgroup delegation: %v", err)
	}

	// Clean up orphaned flatten tmpdirs from prior crashes.
	cleanStaleFlattenDirs()

	mgr := &AppManager{
		containerManager:      containerManager,
		stateBaseDir:          base,
		serviceManager:        serviceManager,
		leadershipState:       make(map[string]cluster.Role),
		lockReader:            lockReader,
		mountVerifier:         defaultMountVerifier,
		observedStatus:        make(map[string]string),
		observedStatusMessage: make(map[string]string),
		oidcHostname:          "piccolo.local",
		runtimeUser:           *runtimeUser,
		syncInFlight:          make(map[string]bool),
	}

	return mgr, nil
}

// SetRouter wires the router registrar for leadership-based routing decisions.
func (m *AppManager) SetRouter(reg router.Registrar) {
	m.stateMu.Lock()
	m.routeRegistrar = reg
	m.stateMu.Unlock()
}

// SetProgressReporter configures the optional progress reporter used for long-running operations.
func (m *AppManager) SetProgressReporter(r events.ProgressReporter) {
	m.stateMu.Lock()
	m.progressReporter = r
	m.stateMu.Unlock()
}

// SetInternalCAPath configures the path to the internal CA certificate on the host.
// This certificate is mounted into containers to enable OIDC back-channel trust.
func (m *AppManager) SetInternalCAPath(path string) {
	m.stateMu.Lock()
	m.internalCAPath = path
	m.stateMu.Unlock()
}

// SetOIDCHostname configures the machine-specific hostname used for OIDC
// back-channel --add-host entries in containers (e.g. "piccolo-abc123.local").
func (m *AppManager) SetOIDCHostname(hostname string) {
	m.stateMu.Lock()
	m.oidcHostname = hostname
	m.stateMu.Unlock()
}

// configureOIDCAuthorizePaths extracts authorize_paths from the app definition
// and wires them on the proxy for OIDC authorize URL rewriting. Always called
// after AllocateForApp/RestoreFromPodman so empty paths propagate as a delete
// to clear any stale state from a previous app version.
func (m *AppManager) configureOIDCAuthorizePaths(instanceID string, def *api.AppDefinition) {
	if m.serviceManager == nil || def == nil {
		return
	}
	var paths []string
	seen := make(map[string]struct{})
	for _, svc := range def.Services {
		if svc.OIDCClient == nil {
			continue
		}
		for _, p := range svc.OIDCClient.AuthorizePaths {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if _, ok := seen[p]; ok {
				continue
			}
			seen[p] = struct{}{}
			paths = append(paths, p)
		}
	}
	m.serviceManager.SetAppOIDCConfig(instanceID, paths)
}

// SetMountVerifier overrides the mount verification callback. Intended for tests.
func (m *AppManager) SetMountVerifier(fn func(string) error) {
	m.stateInitMu.Lock()
	m.mountVerifier = fn
	m.stateInitMu.Unlock()
}

// DiagnosticEphemeralRuntime creates a short-lived podman runtime for storage
// diagnostics. The caller must invoke cleanup() when done.
func (m *AppManager) DiagnosticEphemeralRuntime(ctx context.Context) (container.PodmanRuntime, func(), error) {
	return m.newFlattenRuntime(ctx)
}

// ensureScratchVolume lazy-initializes the shared scratch ephemeral thin LV
// and returns its mount directory. Thread-safe — concurrent callers block
// until the first initialization completes.
func (m *AppManager) ensureScratchVolume(ctx context.Context) (string, error) {
	m.scratchMu.Lock()
	defer m.scratchMu.Unlock()

	if m.scratchAttached {
		return m.scratchHandle.MountDir, nil
	}

	// Read volumeManager under stateMu to avoid a data race with SetVolumeManager.
	m.stateMu.RLock()
	vm := m.volumeManager
	m.stateMu.RUnlock()

	if vm == nil {
		fallback := paths.CoreJoin("tmp")
		log.Printf("WARN: volumeManager nil — falling back to %s for flatten scratch", fallback)
		return fallback, nil
	}

	handle, err := vm.EnsureVolume(ctx, persistence.VolumeRequest{
		ID:    scratchFlattenVolumeID,
		Class: persistence.VolumeClassEphemeral,
	})
	if err != nil {
		return "", fmt.Errorf("ensure scratch volume: %w", err)
	}

	// After an unclean restart the LV may still be attached from the previous
	// process. IsAttachedAdvisory encodes the "reuse only when usable"
	// policy: any non-Attached state (including ambiguous probe) returns
	// false → fall through to a clean Attach.
	if vm.IsAttachedAdvisory(ctx, scratchFlattenVolumeID) {
		cleanStaleDirsIn(handle.MountDir)
		m.scratchHandle = handle
		m.scratchAttached = true
		log.Printf("INFO: scratch flatten volume already attached at %s (reusing)", handle.MountDir)
		return handle.MountDir, nil
	}

	if err := vm.Attach(ctx, handle, persistence.AttachOptions{}); err != nil {
		return "", fmt.Errorf("attach scratch volume: %w", err)
	}

	// Clean up stale flatten-* dirs left by a previous crash. EnsureVolume
	// may return an existing volume (metadata fast-path), so the filesystem
	// can carry over orphaned dirs from prior runs.
	cleanStaleDirsIn(handle.MountDir)

	m.scratchHandle = handle
	m.scratchAttached = true
	log.Printf("INFO: scratch flatten volume attached at %s", handle.MountDir)
	return handle.MountDir, nil
}

// releaseScratchVolume detaches the shared scratch volume. Best-effort —
// logs warnings but never fails shutdown. Does not destroy the LV — cleanup
// and recreation happen lazily on next ensureScratchVolume call.
func (m *AppManager) releaseScratchVolume(ctx context.Context) {
	m.scratchMu.Lock()
	defer m.scratchMu.Unlock()

	if !m.scratchAttached {
		return
	}

	if err := m.volumeManager.Detach(ctx, m.scratchHandle); err != nil {
		log.Printf("WARN: detach scratch volume: %v", err)
	}
	m.scratchAttached = false
	log.Printf("INFO: scratch flatten volume detached")
}

// newFlattenRuntime creates an ephemeral podman runtime backed by the shared
// scratch volume. Returns (runtime, cleanup, error). The cleanup function
// removes only the flatten-* subdirectory, not the shared volume.
func (m *AppManager) newFlattenRuntime(ctx context.Context) (container.PodmanRuntime, func(), error) {
	baseDir, err := m.ensureScratchVolume(ctx)
	if err != nil {
		return container.PodmanRuntime{}, nil, fmt.Errorf("scratch volume: %w", err)
	}
	return newEphemeralFlattenRuntime(baseDir, m.runtimeUser)
}

// SetEventBus configures the event bus for publishing app status change events.
func (m *AppManager) SetEventBus(bus *events.Bus) {
	m.stateMu.Lock()
	m.eventBus = bus
	m.stateMu.Unlock()
}

// SetStateBaseDir overrides the base directory used for filesystem-backed state.
func (m *AppManager) SetStateBaseDir(dir string) {
	base := dir
	if strings.TrimSpace(base) == "" {
		base = paths.CoreRoot()
	}
	clean := filepath.Clean(base)
	m.stateInitMu.Lock()
	if clean != m.stateBaseDir {
		m.stateBaseDir = clean
		m.stateManager = nil
	}
	m.stateInitMu.Unlock()
}

func (m *AppManager) currentRouter() router.Registrar {
	m.stateMu.RLock()
	defer m.stateMu.RUnlock()
	return m.routeRegistrar
}

func (m *AppManager) currentProgressReporter() events.ProgressReporter {
	m.stateMu.RLock()
	defer m.stateMu.RUnlock()
	return m.progressReporter
}

func (m *AppManager) currentEventBus() *events.Bus {
	m.stateMu.RLock()
	defer m.stateMu.RUnlock()
	return m.eventBus
}

// publishAppStatusChanged emits an app status changed event if the event bus is configured.
func (m *AppManager) publishAppStatusChanged(instanceID, status, prevStatus, message string) {
	bus := m.currentEventBus()
	if bus == nil {
		return
	}
	bus.Publish(events.Event{
		Topic: events.TopicAppStatusChanged,
		Payload: events.AppStatusChangedEvent{
			App:        instanceID,
			Status:     status,
			PrevStatus: prevStatus,
			Message:    message,
			Timestamp:  time.Now(),
		},
	})
}

// setObservedStatus updates the in-memory observed status for an app.
// Also clears the transient status message to prevent stale messages
// from persisting across status transitions.
func (m *AppManager) setObservedStatus(instanceID, status string) {
	m.observedStatusMu.Lock()
	m.observedStatus[instanceID] = status
	m.observedStatusMessage[instanceID] = ""
	m.observedStatusMu.Unlock()
}

// getObservedStatus returns the in-memory observed status for an app.
func (m *AppManager) getObservedStatus(instanceID string) string {
	m.observedStatusMu.RLock()
	defer m.observedStatusMu.RUnlock()
	return m.observedStatus[instanceID]
}

// deleteObservedStatus removes the in-memory observed status for an app.
func (m *AppManager) deleteObservedStatus(instanceID string) {
	m.observedStatusMu.Lock()
	delete(m.observedStatus, instanceID)
	delete(m.observedStatusMessage, instanceID)
	m.observedStatusMu.Unlock()
}

// setObservedStatusMessage sets a transient status message and publishes an event if the message changed.
func (m *AppManager) setObservedStatusMessage(instanceID, message string) {
	m.observedStatusMu.Lock()
	prevMessage := m.observedStatusMessage[instanceID]
	m.observedStatusMessage[instanceID] = message
	status := m.observedStatus[instanceID]
	m.observedStatusMu.Unlock()
	if prevMessage != message {
		m.publishAppStatusChanged(instanceID, status, status, message)
	}
}

// getObservedStatusAndMessage atomically reads both status and message for an app.
func (m *AppManager) getObservedStatusAndMessage(instanceID string) (status, message string) {
	m.observedStatusMu.RLock()
	defer m.observedStatusMu.RUnlock()
	return m.observedStatus[instanceID], m.observedStatusMessage[instanceID]
}

// updateStatusWithEvent updates the in-memory observed status and publishes an event if the status changed.
// Clears any transient status message. This should be called within the appropriate lock context
// (reconcileMu for reconciler paths, request-scoped for lifecycle operations).
func (m *AppManager) updateStatusWithEvent(instanceID, newStatus string) {
	m.updateStatusAndMessageWithEvent(instanceID, newStatus, "")
}

// updateStatusAndMessageWithEvent atomically sets both status and message, then publishes an event
// if either the status or message changed. This ensures intra-status message updates (e.g., "Starting
// containers" -> "Re-pulling base image" while status remains "starting") are pushed to SSE clients.
func (m *AppManager) updateStatusAndMessageWithEvent(instanceID, newStatus, message string) {
	m.observedStatusMu.Lock()
	prevStatus := m.observedStatus[instanceID]
	prevMessage := m.observedStatusMessage[instanceID]
	m.observedStatus[instanceID] = newStatus
	m.observedStatusMessage[instanceID] = message
	m.observedStatusMu.Unlock()
	if prevStatus != newStatus || prevMessage != message {
		m.publishAppStatusChanged(instanceID, newStatus, prevStatus, message)
	}
}

// updateStatusPreservingMessageWithEvent updates the status without changing the existing message.
func (m *AppManager) updateStatusPreservingMessageWithEvent(instanceID, newStatus string) {
	m.observedStatusMu.Lock()
	prevStatus := m.observedStatus[instanceID]
	m.observedStatus[instanceID] = newStatus
	msg := m.observedStatusMessage[instanceID]
	m.observedStatusMu.Unlock()
	if prevStatus != newStatus {
		m.publishAppStatusChanged(instanceID, newStatus, prevStatus, msg)
	}
}

func (m *AppManager) emitProgress(ctx context.Context, taskType, instanceID, phase string, progress int, message string, complete bool, opErr error) {
	m.emitProgressWithMetadata(ctx, taskType, instanceID, phase, progress, message, complete, nil, opErr)
}

func (m *AppManager) emitProgressWithMetadata(ctx context.Context, taskType, instanceID, phase string, progress int, message string, complete bool, metadata map[string]any, opErr error) {
	taskID := TaskIDFromContext(ctx)
	if taskID == "" {
		return
	}
	reporter := m.currentProgressReporter()
	if reporter == nil {
		return
	}
	evt := events.TaskProgressEvent{
		TaskID:     taskID,
		TaskType:   taskType,
		InstanceID: instanceID,
		Phase:      phase,
		Progress:   progress,
		Message:    message,
		Metadata:   metadata,
		IsComplete: complete,
		Timestamp:  time.Now().UTC(),
	}
	if opErr != nil {
		evt.Error = opErr.Error()
	}
	reporter.Report(evt)
}

// SetVolumeManager wires the persistence volume manager so apps can use per-app encrypted volumes.
func (m *AppManager) SetVolumeManager(volumes persistence.VolumeManager) {
	m.stateMu.Lock()
	m.volumeManager = volumes
	m.stateMu.Unlock()
}

// SetRootfsManager wires the block-native rootfs volume manager.
func (m *AppManager) SetRootfsManager(rootfs persistence.RootfsVolumeManager) {
	m.stateMu.Lock()
	m.rootfsMgr = rootfs
	m.stateMu.Unlock()
}

// currentRootfsManager returns the rootfs volume manager (may be nil if not configured).
func (m *AppManager) currentRootfsManager() persistence.RootfsVolumeManager {
	m.stateMu.RLock()
	defer m.stateMu.RUnlock()
	return m.rootfsMgr
}

// ObserveRuntimeEvents subscribes to leadership and lock-state events for logging.
func (m *AppManager) ObserveRuntimeEvents(bus *events.Bus) {
	if bus == nil {
		return
	}
	m.eventsMu.Lock()
	if m.eventCancel != nil {
		m.eventCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.eventCancel = cancel
	m.eventsMu.Unlock()

	leaders, cancelLeaders := bus.SubscribeWithCancel(events.TopicLeadershipRoleChanged, 16)
	locks, cancelLocks := bus.SubscribeWithCancel(events.TopicLockStateChanged, 8)
	m.eventsMu.Lock()
	m.eventSubCancels = []func(){cancelLeaders, cancelLocks}
	m.eventsMu.Unlock()
	loopCtx := ctx

	m.eventsWG.Add(1)
	go func() {
		defer m.eventsWG.Done()
		for {
			select {
			case evt, ok := <-leaders:
				if !ok {
					leaders = nil
					if leaders == nil && locks == nil {
						return
					}
					continue
				}
				payload, ok := evt.Payload.(events.LeadershipChanged)
				if !ok {
					log.Printf("WARN: app-manager received unexpected leadership payload: %#v", evt.Payload)
					continue
				}
				m.leadershipMu.Lock()
				m.leadershipState[string(payload.Resource)] = payload.Role
				m.leadershipMu.Unlock()
				log.Printf("INFO: app-manager observed leadership change resource=%s role=%s", payload.Resource, payload.Role)
				m.handleLeadershipChange(loopCtx, payload)
			case evt, ok := <-locks:
				if !ok {
					locks = nil
					if leaders == nil && locks == nil {
						return
					}
					continue
				}
				payload, ok := evt.Payload.(events.LockStateChanged)
				if !ok {
					log.Printf("WARN: app-manager received unexpected lock payload: %#v", evt.Payload)
					continue
				}
				state := "unlocked"
				if payload.Locked {
					state = "locked"
				}
				log.Printf("INFO: app-manager observed control lock state=%s", state)
				if payload.Locked {
					m.markPendingRestore()
				} else {
					// Sequential post-unlock fan-out: restore proxies for
					// already-running containers first, then reconcile toward
					// desired state. Concurrent goroutines were race-safe
					// (per-volume lock + AttachStateAttached short-circuit),
					// but they raced on the same volumes for no benefit and
					// made restore-vs-reconcile ordering observably
					// non-deterministic. Sequencing avoids the redundant
					// volume-attach work the loser would have done, and lets
					// reconcile see the restored proxy state as input.
					go func() {
						m.RestoreServices(loopCtx)
						m.ReconcileOnce(loopCtx)
					}()
				}
			case <-ctx.Done():
				return
			}
			if leaders == nil && locks == nil {
				return
			}
		}
	}()
}

// StopRuntimeEvents stops event observers and waits for goroutines to exit.
// Uses a 10-second timeout to prevent indefinite blocking during shutdown.
func (m *AppManager) StopRuntimeEvents() {
	m.eventsMu.Lock()
	if m.eventCancel != nil {
		m.eventCancel()
		m.eventCancel = nil
	}
	for _, cancel := range m.eventSubCancels {
		cancel()
	}
	m.eventSubCancels = nil
	m.eventsMu.Unlock()

	// Wait with timeout to prevent indefinite blocking
	done := make(chan struct{})
	go func() {
		m.eventsWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		// Goroutines exited cleanly
	case <-time.After(10 * time.Second):
		log.Printf("WARN: StopRuntimeEvents timed out waiting for event goroutines")
	}
}

func (m *AppManager) StartBackground() {
	m.stateMu.Lock()
	if m.reconcileCancel != nil {
		m.stateMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.reconcileCancel = cancel
	m.stateMu.Unlock()

	const interval = 30 * time.Second
	m.reconcileWG.Add(1)
	go func() {
		defer m.reconcileWG.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		m.ReconcileOnce(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.ReconcileOnce(ctx)
			}
		}
	}()

	// Resource stewardship: one-shot startup reconcile of slice policies
	// for all installed apps, then a 5-minute periodic retry loop. The
	// startup pass fixes drift accumulated while piccolod was down (reboot,
	// crash); the periodic pass retries transient failures (systemctl
	// hiccups, /etc/systemd ephemerally unwritable). See plan P1.7.
	m.reconcileWG.Add(1)
	go func() {
		defer m.reconcileWG.Done()
		const slicePolicyInterval = 5 * time.Minute
		m.ReconcileAllSlicePolicies()
		ticker := time.NewTicker(slicePolicyInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.ReconcileAllSlicePolicies()
			}
		}
	}()

	m.startCatalogSyncLoop(ctx)
}

func (m *AppManager) StopBackground() {
	m.stateMu.Lock()
	cancel := m.reconcileCancel
	m.reconcileCancel = nil
	m.stateMu.Unlock()
	if cancel != nil {
		cancel()
	}
	done := make(chan struct{})
	go func() {
		m.reconcileWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		log.Printf("INFO: Background reconciliation stopped cleanly")
	case <-time.After(15 * time.Second):
		log.Printf("WARN: StopBackground timed out after 15s waiting for reconcile goroutine")
	}
}

// StopAllApps stops all running applications and detaches their volumes.
// This is called during graceful shutdown to ensure containers are stopped
// before volumes are unmounted. Apps are stopped in parallel for efficiency.
func (m *AppManager) StopAllApps(ctx context.Context) error {
	log.Printf("INFO: Stopping all running apps for graceful shutdown...")

	// Release the shared scratch volume on all exit paths (best-effort).
	// Uses a fresh context so the unmount runs even if the shutdown ctx expired.
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		m.releaseScratchVolume(releaseCtx)
	}()

	// First, stop the background reconciliation loop
	m.StopBackground()

	// Get the state manager - if unavailable (locked/unmounted), skip app stopping
	// since containers won't be able to access their volumes anyway
	m.stateInitMu.Lock()
	stateMgr := m.stateManager
	m.stateInitMu.Unlock()

	if stateMgr == nil {
		log.Printf("INFO: State manager not initialized, skipping app shutdown")
		return nil
	}

	apps := stateMgr.ListApps()
	if len(apps) == 0 {
		log.Printf("INFO: No apps to stop")
		return nil
	}

	// Separate running apps (need container stop) from stopped apps (only need volume detach)
	var runningApps []*AppInstance
	var stoppedApps []*AppInstance
	for _, app := range apps {
		observed := m.getObservedStatus(app.InstanceID)
		if observed == StatusRunning || observed == StatusStarting {
			runningApps = append(runningApps, app)
		} else {
			stoppedApps = append(stoppedApps, app)
		}
	}

	if len(runningApps) == 0 && len(stoppedApps) == 0 {
		log.Printf("INFO: No apps to process")
		return nil
	}

	var errs []error

	// Stop running apps in parallel with a concurrency limit of 4
	if len(runningApps) > 0 {
		log.Printf("INFO: Stopping %d running apps in parallel...", len(runningApps))

		const maxConcurrency = 4
		sem := make(chan struct{}, maxConcurrency)
		var wg sync.WaitGroup
		var errMu sync.Mutex

		for i, app := range runningApps {
			wg.Add(1)
			go func(idx int, appInst *AppInstance) {
				defer wg.Done()

				// Acquire semaphore
				select {
				case sem <- struct{}{}:
					defer func() { <-sem }()
				case <-ctx.Done():
					errMu.Lock()
					errs = append(errs, fmt.Errorf("app %s: context cancelled before stop", appInst.InstanceID))
					errMu.Unlock()
					return
				}

				log.Printf("INFO: [%d/%d] Stopping app %s...", idx+1, len(runningApps), appInst.InstanceID)

				// Stop container group for this app
				if err := m.stopAppForShutdown(ctx, appInst.InstanceID); err != nil {
					log.Printf("WARN: Failed to stop app %s: %v", appInst.InstanceID, err)
					errMu.Lock()
					errs = append(errs, fmt.Errorf("stop %s: %w", appInst.InstanceID, err))
					errMu.Unlock()
				}

				// Detach the app's encrypted volume
				if err := m.detachAppVolume(ctx, appInst.InstanceID); err != nil {
					log.Printf("WARN: Failed to detach volume for %s: %v", appInst.InstanceID, err)
					errMu.Lock()
					errs = append(errs, fmt.Errorf("detach %s: %w", appInst.InstanceID, err))
					errMu.Unlock()
				}

				log.Printf("INFO: [%d/%d] Stopped app %s", idx+1, len(runningApps), appInst.InstanceID)
			}(i, app)
		}

		wg.Wait()
		log.Printf("INFO: Finished stopping running apps")
	}

	// Detach volumes for stopped apps (they may still have mounted volumes from earlier)
	if len(stoppedApps) > 0 {
		log.Printf("INFO: Detaching volumes for %d stopped apps...", len(stoppedApps))
		for _, app := range stoppedApps {
			if err := m.detachAppVolume(ctx, app.InstanceID); err != nil {
				log.Printf("DEBUG: Volume detach for stopped app %s: %v", app.InstanceID, err)
				// Don't treat as error - volume may already be detached
			}
		}
	}

	log.Printf("INFO: Finished stopping all apps")

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// stopAppForShutdown stops an app's containers without updating state or
// emitting progress events. This is a simplified path for graceful shutdown.
func (m *AppManager) stopAppForShutdown(ctx context.Context, instanceID string) error {
	m.stateInitMu.Lock()
	stateMgr := m.stateManager
	m.stateInitMu.Unlock()

	if stateMgr == nil {
		return nil
	}

	app, exists := stateMgr.GetApp(instanceID)
	if !exists {
		return nil
	}

	def, err := stateMgr.GetAppDefinition(instanceID)
	if err != nil {
		return fmt.Errorf("failed to load app definition: %w", err)
	}

	layout, err := m.ensureAppVolumeLayout(ctx, instanceID)
	if err != nil {
		// Volume might already be detached or unavailable
		log.Printf("DEBUG: Could not get volume layout for %s: %v", instanceID, err)
		return nil
	}

	runtime, err := m.podmanRuntimeForApp(instanceID, layout, piccoloModeFromExtensions(def.Extensions))
	if err != nil {
		return err
	}

	return m.stopContainerGroupWithOpts(ctx, stateMgr, app, def, layout, runtime, stopContainerGroupOpts{
		ShutdownMode: true,
	})
}

// detachAppVolume detaches (unmounts) an app's encrypted volume.
func (m *AppManager) detachAppVolume(ctx context.Context, instanceID string) error {
	volumes := m.currentVolumeManager()
	if volumes == nil {
		return nil
	}

	volID := appVolumeID(instanceID)
	req := persistence.VolumeRequest{ID: volID, Class: persistence.VolumeClassApplication}

	handle, err := volumes.EnsureVolume(ctx, req)
	if err != nil {
		// Volume might not exist or already be detached
		return nil
	}

	return volumes.Detach(ctx, handle)
}

// LastObservedRole returns the most recently observed leadership role for the provided resource.
func (m *AppManager) LastObservedRole(resource string) cluster.Role {
	m.leadershipMu.RLock()
	defer m.leadershipMu.RUnlock()
	if role, ok := m.leadershipState[resource]; ok {
		return role
	}
	return cluster.RoleUnknown
}

func (m *AppManager) ensureUnlocked() error {
	if m.currentLockState() {
		return ErrLocked
	}
	return nil
}

func (m *AppManager) ensureStateManager() (*FilesystemStateManager, error) {
	m.stateInitMu.Lock()
	defer m.stateInitMu.Unlock()
	base := m.stateBaseDir
	if strings.TrimSpace(base) == "" {
		return nil, fmt.Errorf("app manager: state directory not configured")
	}
	if m.stateManager != nil {
		if m.currentLockState() {
			return nil, ErrLocked
		}
		if err := m.ensureMountAvailable(base); err != nil {
			return nil, err
		}
		return m.stateManager, nil
	}
	if m.currentLockState() {
		return nil, ErrLocked
	}
	info, err := os.Stat(base)
	if err != nil {
		return nil, fmt.Errorf("app manager: state directory unavailable: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("app manager: state base %s is not a directory", base)
	}
	if err := m.ensureMountAvailable(base); err != nil {
		return nil, err
	}
	stateMgr, err := NewFilesystemStateManager(base)
	if err != nil {
		return nil, err
	}
	m.stateManager = stateMgr

	// Initialize observed status for all apps on boot.
	// Enabled apps start as StatusStarting until the reconciler confirms StatusRunning.
	for _, app := range stateMgr.ListApps() {
		if app.Enabled {
			m.setObservedStatus(app.InstanceID, StatusStarting)
		} else {
			m.setObservedStatus(app.InstanceID, StatusStopped)
		}
	}

	return stateMgr, nil
}

func (m *AppManager) ensureKernelLeader() error {
	role := m.LastObservedRole(cluster.ResourceKernel)
	if role == cluster.RoleFollower {
		return ErrNotLeader
	}
	return nil
}

func (m *AppManager) handleLeadershipChange(ctx context.Context, change events.LeadershipChanged) {
	switch {
	case change.Resource == cluster.ResourceKernel:
		// No global stop; per-app leadership events drive app lifecycle.
	case strings.HasPrefix(change.Resource, cluster.ResourceAppPrefix):
		appName := strings.TrimPrefix(change.Resource, cluster.ResourceAppPrefix)
		if appName == "" {
			return
		}
		if change.Role == cluster.RoleFollower {
			if err := m.stopForFollowerTransition(ctx, appName); err != nil {
				log.Printf("WARN: follower transition stop app %s failed: %v", appName, err)
			} else {
				// Update observed status immediately so API/UI reflects stopped state.
				m.updateStatusWithEvent(appName, StatusStopped)
			}
		}
		if reg := m.currentRouter(); reg != nil {
			mode := router.ModeLocal
			if change.Role == cluster.RoleFollower {
				mode = router.ModeTunnel
			}
			reg.RegisterAppRoute(appName, mode, "")
		}
	}
}

func (m *AppManager) markPendingRestore() {
	m.restoreMu.Lock()
	m.pendingRestore = true
	m.restoreMu.Unlock()
}

func (m *AppManager) clearPendingRestore() {
	m.restoreMu.Lock()
	m.pendingRestore = false
	m.restoreMu.Unlock()
}

func (m *AppManager) ensureMountAvailable(base string) error {
	if m.mountVerifier == nil {
		m.mountVerifier = defaultMountVerifier
	}
	if m.mountVerifier == nil {
		return nil
	}
	if err := m.mountVerifier(base); err != nil {
		if errors.Is(err, ErrVolumeUnavailable) {
			return ErrVolumeUnavailable
		}
		return err
	}
	return nil
}

func defaultMountVerifier(path string) error {
	if os.Getenv("PICCOLO_ALLOW_UNMOUNTED_TESTS") == "1" {
		if strings.TrimSpace(path) == "" {
			return ErrVolumeUnavailable
		}
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("app manager: state base %s is not a directory", path)
		}
		return nil
	}
	if strings.TrimSpace(path) == "" {
		return ErrVolumeUnavailable
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("app manager: state base %s is not a directory", path)
	}
	parent := filepath.Dir(path)
	if parent == path {
		return nil
	}
	var st, pst unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return err
	}
	if err := unix.Stat(parent, &pst); err != nil {
		return err
	}
	if st.Dev == pst.Dev {
		return ErrVolumeUnavailable
	}
	return nil
}

func (m *AppManager) snapshotApps(allowLocked bool) []*AppInstance {
	state, err := m.ensureStateManager()
	if err != nil {
		if allowLocked && (errors.Is(err, ErrLocked) || errors.Is(err, ErrVolumeUnavailable)) {
			m.stateInitMu.Lock()
			state = m.stateManager
			m.stateInitMu.Unlock()
			if state == nil {
				return nil
			}
		} else {
			if !errors.Is(err, ErrLocked) && !errors.Is(err, ErrVolumeUnavailable) {
				log.Printf("WARN: snapshot apps failed: %v", err)
			}
			return nil
		}
	}
	apps := state.ListApps()
	out := make([]*AppInstance, 0, len(apps))
	for _, app := range apps {
		if app == nil {
			continue
		}
		copy := *app
		out = append(out, &copy)
	}
	return out
}

func (m *AppManager) enterFollower(ctx context.Context) {
	apps := m.snapshotApps(true)
	for _, app := range apps {
		if err := m.stopInternal(ctx, app.InstanceID); err != nil {
			log.Printf("WARN: follower transition stop app %s failed: %v", app.InstanceID, err)
		}
	}
}

// Locked reports the last observed lock state.
func (m *AppManager) Locked() bool {
	return m.currentLockState()
}

// ForceLockState allows tests or orchestration code to override the lock flag directly.
func (m *AppManager) ForceLockState(lock bool) {
	m.lockOverrideMu.Lock()
	defer m.lockOverrideMu.Unlock()
	val := lock
	m.lockOverride = &val
}

// ClearLockOverride removes any explicit override and resumes using the shared reader.
func (m *AppManager) ClearLockOverride() {
	m.lockOverrideMu.Lock()
	defer m.lockOverrideMu.Unlock()
	m.lockOverride = nil
}

// SetLockReader wires a shared lock reader for authoritative lock checks.
func (m *AppManager) SetLockReader(reader LockStateReader) {
	m.lockOverrideMu.Lock()
	m.lockReader = reader
	m.lockOverrideMu.Unlock()
}

func (m *AppManager) currentLockState() bool {
	m.lockOverrideMu.RLock()
	if m.lockOverride != nil {
		locked := *m.lockOverride
		m.lockOverrideMu.RUnlock()
		return locked
	}
	reader := m.lockReader
	m.lockOverrideMu.RUnlock()
	if reader != nil {
		return reader.ControlLocked()
	}
	return false
}

// NewAppManager creates a new filesystem-based app manager with default ServiceManager
func NewAppManager(containerManager ContainerManager, stateDir string) (*AppManager, error) {
	svc := services.NewServiceManager()
	return NewAppManagerWithServices(containerManager, stateDir, svc, nil)
}

// testMountVerifier is a permissive mount verifier for unit tests.
// It only checks that the path exists as a directory — no mount check.
func testMountVerifier(path string) error {
	if strings.TrimSpace(path) == "" {
		return ErrVolumeUnavailable
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("app manager: state base %s is not a directory", path)
	}
	return nil
}

// NewAppManagerForTest creates an AppManager with a synthetic credential for unit tests.
// Skips EnsureXDGRuntimeDir and CheckCgroupDelegation —
// avoids syscall side effects in CI. Uses a permissive mount verifier.
func NewAppManagerForTest(containerManager ContainerManager, stateDir string) (*AppManager, error) {
	base := stateDir
	if strings.TrimSpace(base) == "" {
		base = paths.CoreRoot()
	}
	base = filepath.Clean(base)
	// Use the current process uid/gid so ChownIfNeeded sees files as correctly
	// owned and skips the lchown call (which requires root).
	testCred := &syscall.Credential{Uid: uint32(os.Getuid()), Gid: uint32(os.Getgid())}
	testHome := "/tmp"
	svc := services.NewServiceManager()
	return &AppManager{
		containerManager: containerManager,
		stateBaseDir:     base,
		serviceManager:   svc,
		leadershipState:  make(map[string]cluster.Role),
		mountVerifier:    testMountVerifier,
		observedStatus:   make(map[string]string),
		observedStatusMessage: make(map[string]string),
		oidcHostname: "piccolo.local",
		runtimeUser: container.RuntimeUser{
			Credential: testCred,
			HomeDir:    testHome,
		},
		credentialResolver: func(string) (*syscall.Credential, string, error) {
			return testCred, testHome, nil
		},
		syncInFlight: make(map[string]bool),
	}, nil
}

// NewAppManagerForTestWithServices is like NewAppManagerForTest but with an injected ServiceManager.
func NewAppManagerForTestWithServices(containerManager ContainerManager, stateDir string, serviceManager *services.ServiceManager, lockReader LockStateReader) (*AppManager, error) {
	base := stateDir
	if strings.TrimSpace(base) == "" {
		base = paths.CoreRoot()
	}
	base = filepath.Clean(base)
	testCred := &syscall.Credential{Uid: uint32(os.Getuid()), Gid: uint32(os.Getgid())}
	testHome := "/tmp"
	return &AppManager{
		containerManager: containerManager,
		stateBaseDir:     base,
		serviceManager:   serviceManager,
		leadershipState:  make(map[string]cluster.Role),
		lockReader:       lockReader,
		mountVerifier:    testMountVerifier,
		observedStatus:   make(map[string]string),
		observedStatusMessage: make(map[string]string),
		oidcHostname: "piccolo.local",
		runtimeUser: container.RuntimeUser{
			Credential: testCred,
			HomeDir:    testHome,
		},
		credentialResolver: func(string) (*syscall.Credential, string, error) {
			return testCred, testHome, nil
		},
		syncInFlight: make(map[string]bool),
	}, nil
}

// RestoreServices rebuilds service proxies for running apps based on current container port bindings.
func (m *AppManager) RestoreServices(ctx context.Context) {
	state, err := m.ensureStateManager()
	if err != nil {
		if errors.Is(err, ErrLocked) {
			m.markPendingRestore()
		} else {
			log.Printf("WARN: restore services: state unavailable: %v", err)
		}
		return
	}
	m.clearPendingRestore()
	apps := state.ListApps()
	for _, app := range apps {
		publishCID := strings.TrimSpace(app.PublishContainerID())
		if publishCID == "" {
			continue
		}
		// Respect desired state: disabled apps should not have proxies restored.
		if !app.Enabled {
			if m.serviceManager != nil {
				m.serviceManager.DeactivateApp(app.InstanceID)
			}
			continue
		}
		// Followers should not restore proxies for apps they don't lead.
		if m.LastObservedRole(cluster.ResourceForApp(app.InstanceID)) == cluster.RoleFollower {
			if m.serviceManager != nil {
				m.serviceManager.DeactivateApp(app.InstanceID)
			}
			continue
		}
		def, err := state.GetAppDefinition(app.InstanceID)
		if err != nil {
			log.Printf("WARN: restore services: failed to read app definition for %s: %v", app.InstanceID, err)
			continue
		}
		layout, err := m.ensureAppVolumeLayout(ctx, app.InstanceID)
		if err != nil {
			log.Printf("WARN: restore services: app volume unavailable for %s: %v", app.InstanceID, err)
			continue
		}
		runtime, err := m.podmanRuntimeForApp(app.InstanceID, layout, piccoloModeFromExtensions(def.Extensions))
		if err != nil {
			log.Printf("WARN: restore services: podman runtime unavailable for %s: %v", app.InstanceID, err)
			continue
		}
		ports, err := m.containerManager.InspectPublishedPorts(ctx, runtime, publishCID)
		if err != nil {
			log.Printf("WARN: restore services: podman port inspect failed for %s: %v", app.InstanceID, err)
			continue
		}
		if len(ports) == 0 {
			m.serviceManager.DeactivateApp(app.InstanceID)
			continue
		}
		if _, err := m.serviceManager.RestoreFromPodman(app.InstanceID, def.Listeners, ports); err != nil {
			log.Printf("WARN: restore services: failed to restore proxies for %s: %v", app.InstanceID, err)
			continue
		}
		m.configureOIDCAuthorizePaths(app.InstanceID, def)
		m.serviceManager.SetAppContainerID(app.InstanceID, publishCID)
	}
}

// ReconcileOnce ensures Podman observed state converges to Piccolo desired state.
//
// Desired state is derived from the persisted Enabled flag:
// - Enabled == false => desired stopped
// - Enabled == true => desired running
func (m *AppManager) ReconcileOnce(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()

	if err := m.ensureUnlocked(); err != nil {
		return
	}
	if err := m.ensureKernelLeader(); err != nil {
		return
	}

	state, err := m.ensureStateManager()
	if err != nil {
		return
	}

	// Clean up orphaned per-app users on first reconciliation.
	m.orphanCleanupOnce.Do(func() {
		knownIDs := make(map[string]bool)
		for _, app := range state.ListApps() {
			if app != nil {
				knownIDs[app.InstanceID] = true
			}
		}
		container.CleanupOrphanAppUsers(knownIDs)
	})

	for _, appInst := range state.ListApps() {
		if ctx.Err() != nil {
			return
		}
		if appInst == nil || strings.TrimSpace(appInst.InstanceID) == "" {
			continue
		}
		if err := m.reconcileApp(ctx, state, appInst); err != nil {
			log.Printf("ERROR: reconcile app %s: %v", appInst.InstanceID, err)
		}
	}
}

func (m *AppManager) reconcileApp(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance) error {
	desiredRunning := appInst.Enabled
	if m.LastObservedRole(cluster.ResourceForApp(appInst.InstanceID)) == cluster.RoleFollower {
		desiredRunning = false
	}

	layout, err := m.ensureAppVolumeLayout(ctx, appInst.InstanceID)
	if err != nil {
		return err
	}

	def, err := state.GetAppDefinition(appInst.InstanceID)
	if err != nil {
		return err
	}

	runtime, err := m.podmanRuntimeForApp(appInst.InstanceID, layout, piccoloModeFromExtensions(def.Extensions))
	if err != nil {
		return err
	}

	// Unified reconcile path: all apps (service and workspace) use container groups.
	return m.reconcileContainerGroup(ctx, state, appInst, def, layout, runtime, desiredRunning)
}

func (m *AppManager) ensureServicesForRunningApp(ctx context.Context, def *api.AppDefinition, instanceID, containerID string, runtime container.PodmanRuntime) error {
	if m.serviceManager == nil {
		return nil
	}
	if _, err := m.serviceManager.GetByApp(instanceID); err == nil {
		return nil
	}

	ports, err := m.containerManager.InspectPublishedPorts(ctx, runtime, containerID)
	if err != nil {
		return err
	}
	if len(ports) == 0 {
		if len(def.Listeners) == 0 {
			return nil
		}
		// No published ports observed; allocate fresh endpoints. Publish reconciliation happens separately.
		if _, err := m.serviceManager.AllocateForApp(instanceID, def.Listeners); err != nil {
			return err
		}
		m.configureOIDCAuthorizePaths(instanceID, def)
		m.serviceManager.SetAppContainerID(instanceID, containerID)
		return nil
	}

	if _, err := m.serviceManager.RestoreFromPodman(instanceID, def.Listeners, ports); err != nil {
		return err
	}
	m.configureOIDCAuthorizePaths(instanceID, def)
	m.serviceManager.SetAppContainerID(instanceID, containerID)
	return nil
}

func (m *AppManager) ensurePodmanPublishes(ctx context.Context, def *api.AppDefinition, instanceID, containerID string, runtime container.PodmanRuntime) error {
	if m.serviceManager == nil {
		return nil
	}
	if def == nil {
		return fmt.Errorf("app manager: app definition required to reconcile publishes")
	}
	endpoints, err := m.serviceManager.GetByApp(instanceID)
	if err != nil {
		return nil
	}

	observed, err := m.containerManager.InspectPublishedPorts(ctx, runtime, containerID)
	if err != nil {
		return err
	}

	expected := make(map[string]services.ServiceEndpoint, len(endpoints)) // "port/proto" -> endpoint
	for _, ep := range endpoints {
		key := fmt.Sprintf("%d/%s", ep.GuestPort, ep.Flow.TransportProtocol())
		expected[key] = ep
	}

	// Check if any port reconciliation is needed.
	for key, ep := range expected {
		host, ok := observed[key]
		if !ok || host != ep.HostBind {
			// Podman does not support dynamic port binding updates on running containers.
			// Return error to trigger container recreation.
			return container.ErrPortReconciliationRequired
		}
	}
	for key := range observed {
		if _, ok := expected[key]; !ok {
			// Extra port exists that shouldn't - needs recreation.
			return container.ErrPortReconciliationRequired
		}
	}

	return nil
}

// Install installs a new application instance from its definition.
// Per RFC 20260130, the instanceID is derived from:
// - The primary listener name (for apps with listeners)
// - The workspace_name (for workspace apps without listeners)
func (m *AppManager) Install(ctx context.Context, appDef *api.AppDefinition) (*AppInstance, error) {
	m.reconcileMu.Lock()
	inst, err := m.installLocked(ctx, appDef)
	m.reconcileMu.Unlock()
	// Resource stewardship: derive + apply slice policies for all installed apps.
	// Runs *outside* reconcileMu so the systemctl daemon-reload/set-property
	// calls don't block app lifecycle work. The catalog-sync apply path
	// (catalog_sync_apply.go) calls ReconcileAllSlicePolicies *inside*
	// reconcileMu — that asymmetry is intentional: sync-apply needs the
	// D-9 ordering invariant (slice update strictly before container recreate),
	// while the Install path here has already completed the recreate so
	// ordering is moot. Both are serialized at the sliceReconcileMu layer
	// inside ReconcileAllSlicePolicies itself, so the outer lock-nesting
	// difference is safe.
	// Per D-9: num_active_elastic may have changed, so every elastic app's share
	// needs recompute, not just the newly-installed app.
	if err == nil {
		m.ReconcileAllSlicePolicies()
	}
	return inst, err
}

func (m *AppManager) installLocked(ctx context.Context, appDef *api.AppDefinition) (inst *AppInstance, err error) {
	instanceID := ""
	m.emitProgress(ctx, taskTypeInstallApp, instanceID, taskPhaseValidating, 0, "Validating app manifest", false, nil)
	defer func() {
		if err != nil {
			m.emitProgress(ctx, taskTypeInstallApp, instanceID, taskPhaseComplete, 100, "Install failed", true, err)
			return
		}
		if inst != nil {
			instanceID = inst.InstanceID
		}
		m.emitProgress(ctx, taskTypeInstallApp, instanceID, taskPhaseComplete, 100, "Install complete", true, nil)
	}()

	if err := m.ensureUnlocked(); err != nil {
		return nil, err
	}
	if err := m.ensureKernelLeader(); err != nil {
		return nil, err
	}
	// Set defaults then validate
	SetDefaults(appDef)
	if err := ValidateAppDefinition(appDef); err != nil {
		return nil, fmt.Errorf("invalid app definition: %w", err)
	}

	// Resource-stewardship install-time gate (D-11). Hard-blocks Tier-2
	// (single-app-over-host); logs Tier-1 (soft overshoot) and proceeds.
	if err := m.CheckInstallMemoryGate(appDef); err != nil {
		return nil, err
	}

	state, err := m.ensureStateManager()
	if err != nil {
		return nil, err
	}

	// RFC 20260130: Derive instanceID from primary listener name or workspace_name
	instanceID, err = deriveInstanceID(appDef)
	if err != nil {
		return nil, fmt.Errorf("failed to derive instance ID: %w", err)
	}

	// Validate instance ID format and reserved names (RFC 20260130)
	if err := ValidateInstanceID(instanceID); err != nil {
		return nil, fmt.Errorf("invalid app identity: %w", err)
	}

	// Validate instance ID doesn't collide with existing apps
	existingIDs := state.ListInstanceIDs()
	if err := ValidatePrimaryNameAvailable(instanceID, existingIDs); err != nil {
		return nil, err
	}

	m.emitProgress(ctx, taskTypeInstallApp, instanceID, taskPhaseAllocatingPorts, 10, "Allocating ports", false, nil)
	inst, err = m.installWithRetries(ctx, state, appDef, instanceID, 0)
	return inst, err
}

// deriveInstanceID returns the instance ID from the app definition per RFC 20260130.
// For apps with listeners, it's the primary listener name.
// For workspace apps without listeners, it's the workspace_name.
func deriveInstanceID(appDef *api.AppDefinition) (string, error) {
	if len(appDef.Listeners) > 0 {
		// Find the primary listener (the one with Primary=true, set programmatically)
		for _, l := range appDef.Listeners {
			if l.Primary {
				return l.Name, nil
			}
		}
		// RFC 20260130: All apps with listeners must have Primary=true set on exactly one listener.
		// No fallback - this indicates a bug in install handler if we reach here.
		return "", fmt.Errorf("no primary listener found; this indicates a bug in listener processing")
	}

	// Workspace app without listeners - use workspace_name
	if appDef.WorkspaceName != "" {
		return appDef.WorkspaceName, nil
	}

	return "", fmt.Errorf("cannot derive instance ID: no listeners and no workspace_name")
}

func (m *AppManager) installWithRetries(ctx context.Context, state *FilesystemStateManager, appDef *api.AppDefinition, instanceID string, attempt int) (*AppInstance, error) {
	if attempt >= maxInstallPortRetries {
		return nil, fmt.Errorf("failed to install %s: exhausted host-port retries", instanceID)
	}
	if attempt > 0 {
		m.emitProgress(ctx, taskTypeInstallApp, instanceID, taskPhaseAllocatingPorts, 10, fmt.Sprintf("Retrying installation (attempt %d)", attempt+1), false, nil)
	}

	layout, err := m.ensureAppVolumeLayout(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	runtime, err := m.podmanRuntimeForApp(instanceID, layout, piccoloModeFromExtensions(appDef.Extensions))
	if err != nil {
		// Volume was created but runtime setup failed. Clean up the volume and
		// any partially-created resources (per-app user, runroot).
		m.cleanupInstallResources(instanceID, container.PodmanRuntime{}, appDef)
		return nil, err
	}

	// Clean up volume, podman storage, per-app user on failure.
	// Port retries reuse these resources, so the flag is cleared before recursing.
	cleanupResources := true
	defer func() {
		if cleanupResources {
			m.cleanupInstallResources(instanceID, runtime, appDef)
		}
	}()

	// Allocate services and convert to container spec using instanceID
	endpoints, err := m.serviceManager.AllocateForApp(instanceID, appDef.Listeners)
	if err != nil {
		return nil, fmt.Errorf("failed to allocate service ports: %w", err)
	}
	m.configureOIDCAuthorizePaths(instanceID, appDef)
	cleanupServices := true
	defer func() {
		if cleanupServices {
			m.serviceManager.RemoveApp(instanceID)
		}
	}()

	// Unified install path: all apps (service and workspace) use container groups.
	// Storage preparation (image pull vs workspace disk) is handled inside installContainerGroup.
	m.emitProgress(ctx, taskTypeInstallApp, instanceID, taskPhaseCreatingContainer, 60, "Creating containers", false, nil)
	app, err := m.installContainerGroup(ctx, appDef, instanceID, layout, runtime, endpoints, nil)
	if err != nil {
		var portErr *container.PortInUseError
		if errors.As(err, &portErr) {
			cleanupResources = false // Reuse volume/user on retry
			cleanupServices = false
			m.serviceManager.RemoveApp(instanceID)
			log.Printf("WARN: retrying install for %s due to host port conflict port=%d attempt=%d", instanceID, portErr.Port, attempt)
			if portErr.Port > 0 {
				_ = m.serviceManager.ReserveHostPort(portErr.Port)
			} else {
				for _, ep := range endpoints {
					_ = m.serviceManager.ReserveHostPort(ep.HostBind)
				}
			}
			return m.installWithRetries(ctx, state, appDef, instanceID, attempt+1)
		}
		log.Printf("ERROR: install %s: %v", instanceID, err)
		return nil, err // cleanupResources runs via defer
	}

	m.emitProgress(ctx, taskTypeInstallApp, instanceID, taskPhaseRegisteringServices, 90, "Finalizing installation", false, nil)
	if err := state.StoreApp(app); err != nil {
		// Cleanup all containers if storage fails.
		// Use a detached context — the caller's ctx may be near expiry after
		// a long pull + flatten phase.
		storeCleanupCtx, storeCleanupCancel := context.WithTimeout(context.Background(), cleanupBudget)
		defer storeCleanupCancel()
		if app.NetworkAnchorID != "" {
			_ = m.containerManager.StopContainer(storeCleanupCtx, runtime, app.NetworkAnchorID)
			_ = m.containerManager.RemoveContainer(storeCleanupCtx, runtime, app.NetworkAnchorID)
		}
		for _, cid := range app.Containers {
			_ = m.containerManager.StopContainer(storeCleanupCtx, runtime, cid)
			_ = m.containerManager.RemoveContainer(storeCleanupCtx, runtime, cid)
		}
		// Cleanup rootfs.
		mode := piccoloModeFromExtensions(appDef.Extensions)
		m.detachAllServiceRootfs(storeCleanupCtx, instanceID, mode, appDef, nil)
		m.serviceManager.RemoveApp(instanceID)
		cleanupServices = false
		// cleanupResources runs via defer: destroys volume, runroot, per-app user
		return nil, fmt.Errorf("failed to store app: %w", err)
	}

	// Atomically set observed status and clear any transient message, then populate for API callers.
	m.observedStatusMu.Lock()
	m.observedStatus[instanceID] = StatusRunning
	m.observedStatusMessage[instanceID] = ""
	m.observedStatusMu.Unlock()
	app.Status = StatusRunning
	m.publishAppStatusChanged(instanceID, "installed", "", "")

	cleanupResources = false
	cleanupServices = false
	return app, nil
}

// cleanupInstallResources performs best-effort cleanup of resources created during
// a failed install. This mirrors the cleanup sequence in uninstallLocked to prevent
// orphaned volumes, podman state, and per-app users from leaking on install failure.
//
// Uses a detached context internally — the caller's context may have expired
// (e.g. install timeout), but cleanup must still run to prevent resource leaks.
// The 60s budget fits within the server's shutdown drain window.
func (m *AppManager) cleanupInstallResources(instanceID string, runtime container.PodmanRuntime, appDef *api.AppDefinition) {
	log.Printf("INFO: cleaning up resources for failed install: %s", instanceID)
	// App-logs subtree lives off the data volume, so the DestroyVolume below
	// won't reclaim it — remove it regardless of how this returns.
	defer m.removeAppLogSubtree(instanceID)

	ctx, cancel := context.WithTimeout(context.Background(), cleanupBudget)
	defer cancel()

	// Destroy block-native rootfs if it was partially created.
	// Best-effort: detach + destroy + GC before cleaning up the data volume.
	m.destroyAllServiceRootfs(ctx, instanceID, ModeService, appDef)
	m.destroyAllServiceRootfs(ctx, instanceID, ModeWorkspace, nil)

	// Reset podman storage before destroying the volume (which unmounts the
	// encrypted backing store where podman metadata lives).
	if runtime.Root != "" {
		if err := m.containerManager.ResetStorage(ctx, runtime); err != nil {
			log.Printf("WARN: install cleanup %s: podman storage reset: %v", instanceID, err)
		}
	}

	// Destroy the encrypted volume (detaches, removes ciphertext + mount dir).
	volID := appVolumeID(instanceID)
	if volumes := m.currentVolumeManager(); volumes != nil {
		if err := volumes.DestroyVolume(ctx, volID); err != nil {
			log.Printf("WARN: install cleanup %s: destroy volume: %v", instanceID, err)
		}
	}

	// Remove podman runroot (lives outside the encrypted volume).
	runRoot := runtime.RunRoot
	if runRoot == "" {
		runRoot = filepath.Join(podmanRunRootBase(), volID)
	}
	if err := os.RemoveAll(runRoot); err != nil {
		log.Printf("WARN: install cleanup %s: remove runroot: %v", instanceID, err)
	}

	// Remove per-app service root (graphroot on data partition).
	serviceRoot := runtime.Root
	if serviceRoot == "" {
		serviceRoot = paths.PodmanJoin("apps", instanceID)
	}
	if err := os.RemoveAll(serviceRoot); err != nil {
		log.Printf("WARN: install cleanup %s: remove service root: %v", instanceID, err)
	}

	// Destroy the per-app Linux user. Non-fatal — there's no data left to protect.
	if err := container.DestroyAppUser(instanceID); err != nil {
		log.Printf("WARN: install cleanup %s: destroy per-app user: %v", instanceID, err)
	}
}

// Upsert installs a new application instance.
// Deprecated: With multi-instance support, use Install() directly.
// This method now always creates a new instance (no update behavior).
func (m *AppManager) Upsert(ctx context.Context, appDef *api.AppDefinition) (*AppInstance, error) {
	return m.Install(ctx, appDef)
}

// CloneWorkspace creates a clone of an existing workspace app.
// The clone is a fully independent AppInstance with its own containers, volumes, and per-app user.
// The origin must be a workspace-mode app and must be stopped.
// After cloning, both origin and clone are (re-)started automatically.
func (m *AppManager) CloneWorkspace(ctx context.Context, originID, cloneID string) (*AppInstance, error) {
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()
	return m.cloneWorkspaceLocked(ctx, originID, cloneID)
}

func (m *AppManager) cloneWorkspaceLocked(ctx context.Context, originID, cloneID string) (*AppInstance, error) {
	if err := m.ensureUnlocked(); err != nil {
		return nil, err
	}
	if err := m.ensureKernelLeader(); err != nil {
		return nil, err
	}
	state, err := m.ensureStateManager()
	if err != nil {
		return nil, err
	}

	// Validate origin exists.
	_, exists := state.GetApp(originID)
	if !exists {
		return nil, fmt.Errorf("clone %s from %s: origin not found", cloneID, originID)
	}
	originDef, err := state.GetAppDefinition(originID)
	if err != nil {
		return nil, fmt.Errorf("clone %s from %s: load origin definition: %w", cloneID, originID, err)
	}

	// Validate origin is workspace mode.
	mode := piccoloModeFromExtensions(originDef.Extensions)
	if mode != ModeWorkspace {
		return nil, fmt.Errorf("clone %s from %s: not a workspace app", cloneID, originID)
	}

	// Validate origin is stopped.
	observed := m.getObservedStatus(originID)
	if observed == StatusRunning || observed == StatusStarting {
		return nil, fmt.Errorf("clone %s from %s: origin must be stopped", cloneID, originID)
	}

	// Validate clone name.
	if err := ValidateInstanceID(cloneID); err != nil {
		return nil, fmt.Errorf("clone %s from %s: invalid name: %w", cloneID, originID, err)
	}
	existingIDs := state.ListInstanceIDs()
	if err := ValidatePrimaryNameAvailable(cloneID, existingIDs); err != nil {
		return nil, fmt.Errorf("clone %s from %s: %w", cloneID, originID, err)
	}

	// Set up clone's volume layout and per-app user.
	layout, err := m.ensureAppVolumeLayout(ctx, cloneID)
	if err != nil {
		return nil, fmt.Errorf("clone %s from %s: volume layout: %w", cloneID, originID, err)
	}
	runtime, err := m.podmanRuntimeForApp(cloneID, layout, ModeWorkspace)
	if err != nil {
		m.cleanupInstallResources(cloneID, container.PodmanRuntime{}, originDef)
		return nil, fmt.Errorf("clone %s from %s: podman runtime: %w", cloneID, originID, err)
	}

	// Deferred cleanup: resources, rootfs, services, and containers are cleaned
	// up on failure. Each flag is cleared as ownership transfers to the next step.
	cleanupResources := true
	cleanupRootfs := false
	cleanupServices := false
	var rootfsVolumeID string
	defer func() {
		if cleanupServices {
			m.serviceManager.RemoveApp(cloneID)
		}
		if cleanupRootfs {
			rootfsMgr := m.currentRootfsManager()
			if rootfsMgr != nil {
				// Use a detached context — the caller's ctx may have expired.
				cleanupCtx, cancel := context.WithTimeout(context.Background(), cleanupBudget)
				defer cancel()
				rootfsMgr.DetachRootfs(cleanupCtx, rootfsVolumeID)
				rootfsMgr.DestroyRootfs(cleanupCtx, rootfsVolumeID)
			}
		}
		if cleanupResources {
			m.cleanupInstallResources(cloneID, runtime, originDef)
		}
	}()

	// Build IDMap config from clone's per-app user.
	var idmapPtr *persistence.IDMapConfig
	if runtime.Credential != nil {
		idmap := persistence.IDMapConfig{
			AppUID: runtime.Credential.Uid,
			AppGID: runtime.Credential.Gid,
		}
		username := container.AppUsername(cloneID)
		if subStart, subCount, lookupErr := container.LookupSubUIDRange(username); lookupErr == nil {
			idmap.SubUIDStart = subStart
			idmap.SubUIDCount = subCount
			idmap.SubGIDStart = subStart
			idmap.SubGIDCount = subCount
		} else {
			log.Printf("WARN: clone %s: subuid lookup failed for %s: %v", cloneID, username, lookupErr)
		}
		idmapPtr = &idmap
	}

	// Clone rootfs: thin LV snapshot with clone's IDMap.
	rootfsMgr := m.currentRootfsManager()
	if rootfsMgr == nil {
		return nil, fmt.Errorf("clone %s from %s: rootfs manager not available", cloneID, originID)
	}

	handle, err := rootfsMgr.CloneWorkspace(ctx, originID, cloneID, idmapPtr)
	if err != nil {
		return nil, fmt.Errorf("clone %s from %s: snapshot: %w", cloneID, originID, err)
	}
	rootfsVolumeID = handle.VolumeID
	cleanupRootfs = true

	// Read golden image config from clone's rootfs.
	goldenID := handle.GoldenLV
	imgConfig, err := rootfsMgr.ReadGoldenImageConfig(ctx, goldenID)
	if err != nil {
		return nil, fmt.Errorf("clone %s from %s: read golden image config: %w", cloneID, originID, err)
	}

	// Deep-copy the origin AppDefinition for the clone.
	defJSON, err := json.Marshal(originDef)
	if err != nil {
		return nil, fmt.Errorf("clone %s from %s: marshal definition: %w", cloneID, originID, err)
	}
	var cloneDef api.AppDefinition
	if err := json.Unmarshal(defJSON, &cloneDef); err != nil {
		return nil, fmt.Errorf("clone %s from %s: unmarshal definition: %w", cloneID, originID, err)
	}

	// Update clone definition: workspace_name and primary listener name.
	cloneDef.WorkspaceName = cloneID
	for i := range cloneDef.Listeners {
		if cloneDef.Listeners[i].Primary {
			cloneDef.Listeners[i].Name = cloneID
			break
		}
	}

	// Build prebuiltRootfs map for installContainerGroup.
	primary := primaryServiceFor(&cloneDef, nil)
	prebuiltRootfs := map[string]*rootfsMountInfo{
		primary: {
			handle:    handle,
			imgConfig: imgConfig,
		},
	}

	// Allocate services for the clone.
	endpoints, err := m.serviceManager.AllocateForApp(cloneID, cloneDef.Listeners)
	if err != nil {
		return nil, fmt.Errorf("clone %s from %s: allocate ports: %w", cloneID, originID, err)
	}
	m.configureOIDCAuthorizePaths(cloneID, &cloneDef)
	cleanupServices = true

	// Install the clone's container group with prebuilt rootfs.
	cloneInst, err := m.installContainerGroup(ctx, &cloneDef, cloneID, layout, runtime, endpoints, prebuiltRootfs)
	if err != nil {
		return nil, fmt.Errorf("clone %s from %s: install containers: %w", cloneID, originID, err)
	}

	// Set clone provenance.
	cloneInst.ClonedFrom = originID

	// Persist clone state.
	if err := state.StoreApp(cloneInst); err != nil {
		// Cleanup containers created by installContainerGroup.
		if cloneInst.NetworkAnchorID != "" {
			_ = m.containerManager.StopContainer(ctx, runtime, cloneInst.NetworkAnchorID)
			_ = m.containerManager.RemoveContainer(ctx, runtime, cloneInst.NetworkAnchorID)
		}
		for _, cid := range cloneInst.Containers {
			_ = m.containerManager.StopContainer(ctx, runtime, cid)
			_ = m.containerManager.RemoveContainer(ctx, runtime, cid)
		}
		return nil, fmt.Errorf("clone %s from %s: persist state: %w", cloneID, originID, err)
	}

	// Success — disable deferred cleanup.
	cleanupResources = false
	cleanupRootfs = false
	cleanupServices = false

	// Set clone status to running.
	m.observedStatusMu.Lock()
	m.observedStatus[cloneID] = StatusRunning
	m.observedStatusMessage[cloneID] = ""
	m.observedStatusMu.Unlock()
	cloneInst.Status = StatusRunning
	m.publishAppStatusChanged(cloneID, "installed", "", "")

	// Restart origin (best-effort).
	if restartErr := m.startLocked(ctx, originID); restartErr != nil {
		log.Printf("WARN: clone %s: failed to restart origin %s: %v", cloneID, originID, restartErr)
	}

	return cloneInst, nil
}

// ListWorkspaceClones returns all apps that were cloned from the given origin.
func (m *AppManager) ListWorkspaceClones(ctx context.Context, originID string) ([]*AppInstance, error) {
	state, err := m.ensureStateManager()
	if err != nil {
		return nil, err
	}

	// Validate origin exists.
	if _, exists := state.GetApp(originID); !exists {
		return nil, fmt.Errorf("app instance not found: %s", originID)
	}

	rootfsMgr := m.currentRootfsManager()
	if rootfsMgr == nil {
		return nil, nil
	}
	originVolumeID := "ws-" + originID
	cloneVolumeIDs, err := rootfsMgr.ListClones(ctx, originVolumeID)
	if err != nil {
		return nil, fmt.Errorf("list clones for %s: %w", originID, err)
	}

	var clones []*AppInstance
	for _, volID := range cloneVolumeIDs {
		// Strip "ws-" prefix to get instanceID.
		instanceID := strings.TrimPrefix(volID, "ws-")
		cached, exists := state.GetApp(instanceID)
		if !exists {
			continue
		}
		clone := *cached
		clone.Status, clone.StatusMessage = m.getObservedStatusAndMessage(instanceID)
		if clone.Status == "" {
			clone.Status = StatusStopped
		}
		clones = append(clones, &clone)
	}
	return clones, nil
}

// List returns all installed applications.
// Returns shallow copies of cached instances to avoid data races when setting Status.
func (m *AppManager) List(ctx context.Context) ([]*AppInstance, error) {
	state, err := m.ensureStateManager()
	if err != nil {
		return nil, err
	}
	cached := state.ListApps()
	// Return shallow copies to avoid mutating cached instances (data race on Status field).
	apps := make([]*AppInstance, len(cached))
	for i, app := range cached {
		copy := *app
		copy.Status, copy.StatusMessage = m.getObservedStatusAndMessage(app.InstanceID)
		if copy.Status == "" {
			copy.Status = StatusStopped
		}
		apps[i] = &copy
	}
	return apps, nil
}

// Get returns a specific application instance by instanceID.
// Returns a shallow copy of the cached instance to avoid data races when setting Status.
func (m *AppManager) Get(ctx context.Context, instanceID string) (*AppInstance, error) {
	state, err := m.ensureStateManager()
	if err != nil {
		return nil, err
	}
	cached, exists := state.GetApp(instanceID)
	if !exists {
		return nil, fmt.Errorf("app instance not found: %s", instanceID)
	}
	// Return shallow copy to avoid mutating cached instance (data race on Status field).
	app := *cached
	app.Status, app.StatusMessage = m.getObservedStatusAndMessage(instanceID)
	if app.Status == "" {
		app.Status = StatusStopped
	}
	return &app, nil
}

// GetAppDefinition returns the full definition (app.yaml content) for an installed app instance.
func (m *AppManager) GetAppDefinition(ctx context.Context, instanceID string) (*api.AppDefinition, error) {
	state, err := m.ensureStateManager()
	if err != nil {
		return nil, err
	}
	return state.GetAppDefinition(instanceID)
}

// Start starts an application instance by instanceID.
func (m *AppManager) Start(ctx context.Context, instanceID string) error {
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()
	return m.startLocked(ctx, instanceID)
}

func (m *AppManager) startLocked(ctx context.Context, instanceID string) (err error) {
	m.emitProgress(ctx, taskTypeStartApp, instanceID, taskPhaseStarting, 0, "Starting app", false, nil)
	defer func() {
		if err != nil {
			m.emitProgress(ctx, taskTypeStartApp, instanceID, taskPhaseComplete, 100, "Start failed", true, err)
			return
		}
		m.emitProgress(ctx, taskTypeStartApp, instanceID, taskPhaseComplete, 100, "Started", true, nil)
	}()

	if err := m.ensureUnlocked(); err != nil {
		return err
	}
	if err := m.ensureKernelLeader(); err != nil {
		return err
	}
	state, err := m.ensureStateManager()
	if err != nil {
		return err
	}
	app, exists := state.GetApp(instanceID)
	if !exists {
		return fmt.Errorf("app instance not found: %s", instanceID)
	}

	layout, err := m.ensureAppVolumeLayout(ctx, instanceID)
	if err != nil {
		return err
	}

	def, defErr := state.GetAppDefinition(instanceID)
	if defErr != nil {
		return fmt.Errorf("failed to load app definition: %w", defErr)
	}

	runtime, err := m.podmanRuntimeForApp(instanceID, layout, piccoloModeFromExtensions(def.Extensions))
	if err != nil {
		return err
	}

	// Unified start path for all app modes (container group: network anchor + services)
	m.emitProgress(ctx, taskTypeStartApp, instanceID, taskPhaseStarting, 60, "Starting containers", false, nil)
	return m.startContainerGroup(ctx, state, app, def, layout, runtime)
}

// Stop stops an application instance by instanceID.
func (m *AppManager) Stop(ctx context.Context, instanceID string) error {
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()

	if err := m.ensureUnlocked(); err != nil {
		return err
	}
	if err := m.ensureKernelLeader(); err != nil {
		return err
	}
	return m.stopInternal(ctx, instanceID)
}

func (m *AppManager) stopForFollowerTransition(ctx context.Context, instanceID string) error {
	state, err := m.ensureStateManager()
	if err != nil {
		return err
	}
	app, exists := state.GetApp(instanceID)
	if !exists {
		return nil
	}

	layout, err := m.ensureAppVolumeLayout(ctx, instanceID)
	if err != nil {
		return err
	}

	def, defErr := state.GetAppDefinition(instanceID)
	mode := ModeService
	if defErr == nil {
		mode = piccoloModeFromExtensions(def.Extensions)
	}

	runtime, err := m.podmanRuntimeForApp(instanceID, layout, mode)
	if err != nil {
		return err
	}

	if defErr == nil && mode == ModeService {
		primary := primaryServiceFor(def, app)
		order, _ := serviceStartOrder(def.Services)
		for i := len(order) - 1; i >= 0; i-- {
			svcName := order[i]
			cid := strings.TrimSpace(app.Containers[svcName])
			if cid == "" {
				name := containerNameForService(instanceID, svcName, primary)
				if id, err := m.containerManager.ResolveContainerIDByName(ctx, runtime, name); err == nil {
					cid = id
				}
			}
			if cid == "" {
				continue
			}
			if err := m.containerManager.StopContainer(ctx, runtime, cid); err != nil {
				var notFound *container.ContainerNotFoundError
				if !errors.As(err, &notFound) {
					return err
				}
			}
		}

		anchorID := strings.TrimSpace(app.NetworkAnchorID)
		if anchorID == "" {
			if id, err := m.containerManager.ResolveContainerIDByName(ctx, runtime, networkAnchorContainerName(instanceID)); err == nil {
				anchorID = id
			}
		}
		if anchorID != "" {
			if err := m.containerManager.StopContainer(ctx, runtime, anchorID); err != nil {
				var notFound *container.ContainerNotFoundError
				if !errors.As(err, &notFound) {
					return err
				}
			}
		}

		if m.serviceManager != nil {
			m.serviceManager.DeactivateApp(instanceID)
		}
		return nil
	}

	if err := m.containerManager.StopContainer(ctx, runtime, app.PrimaryContainerID()); err != nil {
		var notFound *container.ContainerNotFoundError
		if !errors.As(err, &notFound) {
			return err
		}
	}
	if m.serviceManager != nil {
		m.serviceManager.DeactivateApp(instanceID)
	}
	return nil
}

func (m *AppManager) stopInternal(ctx context.Context, instanceID string) (err error) {
	m.emitProgress(ctx, taskTypeStopApp, instanceID, taskPhaseStopping, 0, "Stopping app", false, nil)
	defer func() {
		if err != nil {
			m.emitProgress(ctx, taskTypeStopApp, instanceID, taskPhaseComplete, 100, "Stop failed", true, err)
			return
		}
		m.emitProgress(ctx, taskTypeStopApp, instanceID, taskPhaseComplete, 100, "Stopped", true, nil)
	}()

	state, err := m.ensureStateManager()
	if err != nil {
		return err
	}
	app, exists := state.GetApp(instanceID)
	if !exists {
		return fmt.Errorf("app instance not found: %s", instanceID)
	}

	layout, err := m.ensureAppVolumeLayout(ctx, instanceID)
	if err != nil {
		return err
	}

	def, defErr := state.GetAppDefinition(instanceID)
	if defErr != nil {
		return fmt.Errorf("failed to load app definition: %w", defErr)
	}

	runtime, err := m.podmanRuntimeForApp(instanceID, layout, piccoloModeFromExtensions(def.Extensions))
	if err != nil {
		return err
	}

	// Unified stop path for all app modes (container group: network anchor + services)
	m.emitProgress(ctx, taskTypeStopApp, instanceID, taskPhaseStopping, 40, "Stopping containers", false, nil)
	return m.stopContainerGroup(ctx, state, app, def, layout, runtime)
}

// Uninstall removes an application instance completely by instanceID,
// including all container data, encrypted volumes, and podman state.
func (m *AppManager) Uninstall(ctx context.Context, instanceID string) error {
	// Remove the slice drop-in before the user is destroyed. Live-reset is
	// unnecessary because the slice itself is about to be torn down.
	m.RemoveSlicePolicyForApp(instanceID)

	m.reconcileMu.Lock()
	err := m.uninstallLocked(ctx, instanceID)
	m.reconcileMu.Unlock()

	// Recompute slice policies for remaining apps: num_active_elastic may
	// have changed. Runs outside reconcileMu (systemctl calls).
	if err == nil {
		m.ReconcileAllSlicePolicies()
	}
	return err
}

func (m *AppManager) uninstallLocked(ctx context.Context, instanceID string) (err error) {
	m.emitProgress(ctx, taskTypeUninstallApp, instanceID, taskPhaseStopping, 0, "Stopping app", false, nil)
	defer func() {
		if err != nil {
			m.emitProgress(ctx, taskTypeUninstallApp, instanceID, taskPhaseComplete, 100, "Uninstall failed", true, err)
			return
		}
		m.emitProgress(ctx, taskTypeUninstallApp, instanceID, taskPhaseComplete, 100, "Uninstalled", true, nil)
	}()
	// Remove the app-logs subtree only on a SUCCESSFUL uninstall. If this
	// returns early (locked, follower, container teardown or DestroyVolume
	// error) the app is still installed — keep its logs for diagnosis. A
	// success-then-crash orphan is reaped at the next unlock.
	defer func() {
		if err == nil {
			m.removeAppLogSubtree(instanceID)
		}
	}()

	if err := m.ensureUnlocked(); err != nil {
		return err
	}
	if err := m.ensureKernelLeader(); err != nil {
		return err
	}
	state, err := m.ensureStateManager()
	if err != nil {
		return err
	}
	app, exists := state.GetApp(instanceID)
	if !exists {
		return fmt.Errorf("app instance not found: %s", instanceID)
	}

	// Mark as uninstalling early so concurrent readers (log streams, exec) get a clean rejection
	// instead of racing against infrastructure teardown. Rollback on failure so the app doesn't
	// get stuck in "uninstalling" permanently.
	prevStatus := m.getObservedStatus(instanceID)
	m.updateStatusWithEvent(instanceID, StatusUninstalling)
	defer func() {
		if err != nil {
			rollback := prevStatus
			if rollback == "" {
				rollback = StatusStopped
			}
			m.updateStatusWithEvent(instanceID, rollback)
		}
	}()

	layout, err := m.ensureAppVolumeLayout(ctx, instanceID)
	if err != nil {
		return err
	}

	def, defErr := state.GetAppDefinition(instanceID)
	if defErr != nil {
		return fmt.Errorf("failed to load app definition: %w", defErr)
	}

	runtime, err := m.podmanRuntimeForApp(instanceID, layout, piccoloModeFromExtensions(def.Extensions))
	if err != nil {
		return err
	}

	// Unified uninstall path for all app modes (container group: network anchor + services)
	m.emitProgress(ctx, taskTypeUninstallApp, instanceID, taskPhaseRemovingContainer, 40, "Removing containers", false, nil)
	if err := m.uninstallContainerGroup(ctx, app, def, layout, runtime); err != nil {
		return err
	}

	// Destroy block-native rootfs if applicable (before volume destroy).
	mode := piccoloModeFromExtensions(def.Extensions)
	m.destroyAllServiceRootfs(ctx, instanceID, mode, def)

	// Reset podman storage BEFORE unmounting the volume.
	// This allows podman to properly clean its metadata files (db.sql, locks, etc.)
	// which live inside the encrypted volume.
	m.emitProgress(ctx, taskTypeUninstallApp, instanceID, taskPhaseCleaningVolumes, 80, "Purging app data", false, nil)
	if err := m.containerManager.ResetStorage(ctx, runtime); err != nil {
		log.Printf("WARN: podman storage reset for %s failed: %v", instanceID, err)
	}

	// Destroy the volume (detaches, then removes ciphertext, metadata, mount directory)
	volID := appVolumeID(instanceID)
	volumes := m.currentVolumeManager()
	if volumes == nil {
		return fmt.Errorf("volume manager not available")
	}
	if err := volumes.DestroyVolume(ctx, volID); err != nil {
		return fmt.Errorf("failed to purge app data: %w", err)
	}

	// Remove podman runRoot which lives outside the encrypted volume
	if err := os.RemoveAll(runtime.RunRoot); err != nil {
		log.Printf("WARN: failed to remove podman runRoot %s: %v", runtime.RunRoot, err)
	}

	// Destroy the per-app Linux user. Non-fatal — the user has no data left to access.
	if err := container.DestroyAppUser(instanceID); err != nil {
		log.Printf("WARN: failed to destroy per-app user for %s: %v", instanceID, err)
	}

	// Remove from filesystem and cache (state only)
	if err := state.RemoveApp(instanceID); err != nil {
		return fmt.Errorf("failed to remove app from storage: %w", err)
	}

	// Clean up observed status and emit "uninstalled" event.
	// Status is deterministically StatusUninstalling here (set at entry, no rollback on success).
	m.deleteObservedStatus(instanceID)
	m.publishAppStatusChanged(instanceID, "uninstalled", StatusUninstalling, "")

	return nil
}

// UpdateImage re-pulls every non-digest-pinned service image for an app
// instance and rebuilds the rootfs for any service whose registry-resolved
// manifest digest has drifted. The app manifest is not modified — tag refs
// stay the same; only the underlying rootfs LV gets refreshed.
func (m *AppManager) UpdateImage(ctx context.Context, instanceID string) error {
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()
	return m.updateImageLocked(ctx, instanceID)
}

func (m *AppManager) updateImageLocked(ctx context.Context, instanceID string) (err error) {
	m.emitProgress(ctx, taskTypeUpdateImage, instanceID, taskPhaseValidating, 0, "Validating update", false, nil)
	defer func() {
		if err != nil {
			m.emitProgress(ctx, taskTypeUpdateImage, instanceID, taskPhaseComplete, 100, "Update failed", true, err)
		} else {
			m.emitProgress(ctx, taskTypeUpdateImage, instanceID, taskPhaseComplete, 100, "Update complete", true, nil)
		}
	}()

	if err := m.ensureUnlocked(); err != nil {
		return err
	}
	if err := m.ensureKernelLeader(); err != nil {
		return err
	}
	if m.serviceManager == nil {
		return fmt.Errorf("app manager: service manager not configured")
	}
	state, err := m.ensureStateManager()
	if err != nil {
		return err
	}
	appInst, exists := state.GetApp(instanceID)
	if !exists {
		return fmt.Errorf("app instance not found: %s", instanceID)
	}
	layout, err := m.ensureAppVolumeLayout(ctx, instanceID)
	if err != nil {
		return err
	}

	curDef, err := state.GetAppDefinition(instanceID)
	if err != nil {
		return fmt.Errorf("failed to read current app.yaml: %w", err)
	}

	mode := piccoloModeFromExtensions(curDef.Extensions)
	if mode == ModeWorkspace {
		return fmt.Errorf("cannot update image for workspace apps: workspace persistence is tied to the base image; uninstall and reinstall to use a different base image")
	}
	if mode != ModeService {
		return fmt.Errorf("image update not supported for mode %q", mode)
	}

	runtime, err := m.podmanRuntimeForApp(instanceID, layout, mode)
	if err != nil {
		return err
	}

	// Services whose digest didn't drift short-circuit inside updateServiceModeImage.
	updatedImages := make(map[string]string)
	for svcName, svc := range curDef.Services {
		if svc.Image == "" || isDigestPinned(svc.Image) {
			continue
		}
		updatedImages[svcName] = svc.Image
	}
	if len(updatedImages) == 0 {
		log.Printf("INFO: update image %s: all service images are digest-pinned; nothing to re-pull", instanceID)
		return nil
	}

	// RollbackToSnapshot reads app.prev.yaml to restore pre-update state — keep
	// it in sync even though Update itself doesn't mutate the manifest.
	if err := state.BackupCurrentAppDefinition(instanceID); err != nil {
		return fmt.Errorf("backup app.yaml: %w", err)
	}

	// Service-mode: transactional rootfs update (RFC 20260302).
	if err := m.updateServiceModeImage(ctx, state, appInst, curDef, layout, runtime, updatedImages); err != nil {
		return err
	}
	// Image update succeeded — clear catalog manifest sync throttle so
	// the next sync tick re-classifies the catalog version (which may
	// have been blocked as DiffKindImageOnly or DiffKindStructuralWithImage).
	if appInst.LastSyncAttemptHash != "" || appInst.LastSyncError != "" {
		appInst.LastSyncAttemptHash = ""
		appInst.LastSyncError = ""
		if err := state.StoreAppMetadata(appInst); err != nil {
			log.Printf("WARN: update image %s: clear sync throttle: %v", instanceID, err)
		}
	}
	return nil
}

// isDigestPinned: refs of form "name@digest" are immutable, so re-pulling
// them cannot produce a new digest.
func isDigestPinned(img string) bool {
	return strings.Contains(img, "@")
}

// dataVolumeSnapshotter is a narrow interface for data volume snapshot operations.
// Satisfied by luksVolumeManager via type assertion on m.volumeManager.
type dataVolumeSnapshotter interface {
	SnapshotDataVolume(ctx context.Context, instanceID, snapshotLVName string) error
	DestroyDataSnapshot(ctx context.Context, snapshotLVName string) error
}

// dataVolumeRollbacker is a narrow interface for data volume rollback operations.
// Satisfied by luksVolumeManager via type assertion on m.volumeManager.
type dataVolumeRollbacker interface {
	// RollbackDataVolume performs a LUKS-aware LV rename swap with full detach/attach cycle.
	// Returns (renamesCommitted, snapshotPromoted, error):
	// (false, false, err) — failed before LV renames, no LV state change
	// (true, false, err) — active→failed rename committed, but snapshot→active failed
	// (true, true, nil) — fully succeeded (both renames + re-attach)
	// (true, true, err) — both renames committed but re-attach failed
	RollbackDataVolume(ctx context.Context, instanceID string, snapshotLVName, failedLVName string) (renamesCommitted, snapshotPromoted bool, err error)
}

// updateServiceModeImage performs a transactional rootfs update for service-mode apps (RFC 20260302).
// Handles multi-container apps: changed services get new rootfs, unchanged services reuse existing.
// All containers (including anchor) are stopped, snapshotted, removed, and recreated.
func (m *AppManager) updateServiceModeImage(
	ctx context.Context,
	state *FilesystemStateManager,
	appInst *AppInstance,
	newDef *api.AppDefinition,
	layout appVolumeLayout,
	runtime container.PodmanRuntime,
	updatedImages map[string]string, // svcName → newImage (only changed services)
) error {
	instanceID := appInst.InstanceID
	curDef := appInst.Definition
	primary := primaryServiceFor(newDef, appInst)
	mode := piccoloModeFromExtensions(newDef.Extensions)

	rootfs := m.currentRootfsManager()
	if rootfs == nil {
		return fmt.Errorf("rootfs volume manager not configured")
	}

	// Create ephemeral runtime for image pull + inspect (digest derivation).
	ephRT, ephCleanup, ephErr := m.newFlattenRuntime(ctx)
	if ephErr != nil {
		return fmt.Errorf("create ephemeral runtime: %w", ephErr)
	}
	defer ephCleanup()

	m.emitProgress(ctx, taskTypeUpdateImage, instanceID, taskPhasePullingImage, 10, "Pulling new image", false, nil)

	// 1. For each changed service: pull + inspect image → get digest → compute versioned volume ID.
	type changedService struct {
		svcName       string
		newImage      string
		digest        string
		volumeID      string
		imageSizeHint int64
		handle        persistence.RootfsHandle
		imgConfig     persistence.GoldenImageConfig
	}
	changed := make([]changedService, 0, len(updatedImages))
	for svcName, newImage := range updatedImages {
		if pullErr := m.containerManager.PullImage(ctx, ephRT, newImage); pullErr != nil {
			return fmt.Errorf("pull image %s (service %s): %w", newImage, svcName, pullErr)
		}
		imgConfig, inspErr := m.containerManager.InspectImage(ctx, ephRT, newImage)
		if inspErr != nil {
			return fmt.Errorf("inspect image %s (service %s): %w", newImage, svcName, inspErr)
		}
		digest := ""
		if len(imgConfig.RepoDigests) > 0 {
			digest = imgConfig.RepoDigests[0]
		} else {
			digest = imgConfig.Digest
		}
		shortDigest := persistence.ShortDigest(digest)
		volID := persistence.VersionedServiceRootfsVolumeID(instanceID, svcName, shortDigest)
		changed = append(changed, changedService{
			svcName:       svcName,
			newImage:      newImage,
			digest:        digest,
			volumeID:      volID,
			imageSizeHint: imgConfig.Size,
		})
	}

	// 2. Build IDMap config (once, shared across all services).
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
			idmap.SubGIDStart = subStart
			idmap.SubGIDCount = subCount
		} else {
			log.Printf("WARN: update %s: subuid lookup failed for %s: %v", instanceID, username, lookupErr)
		}
	}

	m.emitProgress(ctx, taskTypeUpdateImage, instanceID, taskPhaseCreatingRootfs, 30, "Creating root filesystem", false, nil)

	// 3. For each changed service: create new rootfs (idempotent, while still running).
	var err error
	for i := range changed {
		cs := &changed[i]
		if rootfs.RootfsExists(cs.volumeID) {
			log.Printf("INFO: update %s: rootfs %s already exists, attaching", instanceID, cs.volumeID)
			cs.handle, err = rootfs.AttachRootfs(ctx, cs.volumeID)
			if err != nil {
				return fmt.Errorf("attach existing rootfs %s: %w", cs.volumeID, err)
			}
		} else {
			cs.handle, err = rootfs.CreateServiceRootfs(ctx, persistence.ServiceRootfsRequest{
				InstanceID:    instanceID,
				ServiceName:   cs.svcName,
				ImageDigest:   cs.digest,
				ImageRef:      cs.newImage,
				IDMap:         idmap,
				VolumeID:      cs.volumeID,
				ImageSizeHint: cs.imageSizeHint,
				PrePulledDir:  filepath.Dir(ephRT.Root),
			})
			if err != nil {
				return fmt.Errorf("create rootfs for service %s: %w", cs.svcName, err)
			}
		}
	}

	// 4. Read image config from golden LV for each changed service.
	for i := range changed {
		cs := &changed[i]
		goldenCfg, cfgErr := m.readImageConfigForRootfs(ctx, rootfs, cs.digest)
		if cfgErr != nil {
			log.Printf("WARN: update %s: failed to read image config for %s: %v", instanceID, cs.svcName, cfgErr)
		} else {
			cs.imgConfig = goldenCfg
		}
	}

	m.emitProgress(ctx, taskTypeUpdateImage, instanceID, taskPhaseStopping, 50, "Stopping containers", false, nil)

	// 5. Stop ALL containers (services in reverse dep order, then anchor).
	if err := m.stopContainersForMultiApp(ctx, appInst, curDef, runtime); err != nil {
		log.Printf("WARN: update %s: stop containers: %v", instanceID, err)
	}

	m.emitProgress(ctx, taskTypeUpdateImage, instanceID, taskPhaseSnapshotting, 55, "Creating rollback snapshot", false, nil)

	// 6. Tuple snapshot: capture pre-update state for rollback.
	tupleState, snapshotOK := m.snapshotTupleBeforeUpdate(ctx, state, appInst, primary)
	if !snapshotOK {
		log.Printf("WARN: update %s: proceeding without rollback capability", instanceID)
	}

	m.emitProgress(ctx, taskTypeUpdateImage, instanceID, taskPhaseRemovingContainer, 60, "Removing containers", false, nil)

	// 7. Remove ALL containers (services + anchor).
	if err := m.removeContainersForMultiApp(ctx, appInst, curDef, runtime); err != nil {
		log.Printf("WARN: update %s: remove containers: %v", instanceID, err)
	}

	m.emitProgress(ctx, taskTypeUpdateImage, instanceID, taskPhaseRecreatingContainer, 70, "Recreating containers", false, nil)

	// 8. Recreate ALL containers (anchor + services in start order).
	// Build rootfs map: changed services use new rootfs, unchanged use existing.
	changedMap := make(map[string]*rootfsMountInfo, len(changed))
	for i := range changed {
		cs := &changed[i]
		changedMap[cs.svcName] = &rootfsMountInfo{handle: cs.handle, imgConfig: cs.imgConfig}
	}

	// Attach unchanged service rootfs volumes.
	unchangedRootfs, err := m.ensureAllServiceRootfsAttached(ctx, instanceID, mode, newDef, appInst)
	if err != nil {
		// Unchanged rootfs attach failed — abort to prevent metadata/rootfs mismatch.
		// Detach new rootfs volumes; reconciler handles recovery using old definition.
		for _, cs := range changed {
			_ = rootfs.DetachRootfs(ctx, cs.volumeID)
		}
		m.setObservedStatus(instanceID, StatusError)
		return fmt.Errorf("update %s: attach unchanged rootfs: %w", instanceID, err)
	}

	// Merge: changed services override unchanged.
	prebuiltRootfs := make(map[string]*rootfsMountInfo, len(newDef.Services))
	for svcName, info := range unchangedRootfs {
		prebuiltRootfs[svcName] = info
	}
	for svcName, info := range changedMap {
		prebuiltRootfs[svcName] = info
	}

	// Get current service endpoints (ports should not change during update).
	endpoints, _ := m.serviceManager.GetByApp(instanceID)
	if len(endpoints) == 0 {
		// Fallback: if service registry lost (e.g., error escalation), allocate fresh endpoints.
		var allocErr error
		endpoints, allocErr = m.serviceManager.AllocateForApp(instanceID, newDef.Listeners)
		if allocErr != nil {
			return fmt.Errorf("update %s: allocate endpoints: %w", instanceID, allocErr)
		}
	}
	// Update OIDC authorize paths from the new manifest on both paths
	// (endpoints preserved or freshly allocated) — an app update may add,
	// remove, or change authorize_paths and the proxy must reflect the new version.
	m.configureOIDCAuthorizePaths(instanceID, newDef)
	result, err := m.installContainerGroup(ctx, newDef, instanceID, layout, runtime, endpoints, prebuiltRootfs)
	if err != nil {
		// Detach new rootfs volumes, leave old ones for recovery.
		for _, cs := range changed {
			_ = rootfs.DetachRootfs(ctx, cs.volumeID)
		}
		m.setObservedStatus(instanceID, StatusError)
		return fmt.Errorf("recreate containers after update: %w", err)
	}

	m.emitProgress(ctx, taskTypeUpdateImage, instanceID, taskPhaseFinalizing, 90, "Saving state", false, nil)

	// 9. Update state: ActiveRootfs for changed services, definition, container IDs.
	if appInst.ActiveRootfs == nil {
		appInst.ActiveRootfs = make(map[string]string)
	}
	for _, cs := range changed {
		appInst.ActiveRootfs[cs.svcName] = cs.volumeID
	}
	appInst.Definition = newDef
	appInst.PrimaryService = result.PrimaryService
	appInst.NetworkAnchorID = result.NetworkAnchorID
	appInst.Containers = result.Containers
	appInst.UpdatedAt = time.Now()
	if err := state.StoreApp(appInst); err != nil {
		// Best-effort cleanup: remove containers created by installContainerGroup
		// to prevent unmanaged containers running with stale on-disk metadata.
		if rmErr := m.removeContainersForMultiApp(ctx, result, newDef, runtime); rmErr != nil {
			log.Printf("WARN: update %s: cleanup after persist failure: %v", instanceID, rmErr)
		}
		for _, cs := range changed {
			_ = rootfs.DetachRootfs(ctx, cs.volumeID)
		}
		m.setObservedStatus(instanceID, StatusError)
		return fmt.Errorf("store app: %w", err)
	}

	// 10. Post-update: record new active generation (only when snapshot exists for rollback).
	if tupleState != nil && snapshotOK {
		m.recordPostUpdateGeneration(state, appInst, tupleState)
	}

	m.setObservedStatus(instanceID, StatusRunning)
	log.Printf("INFO: update %s: image updated for %d service(s)", instanceID, len(changed))
	return nil
}

// snapshotTupleBeforeUpdate captures pre-update state for rollback (RFC 20260302 Phase 2).
// Returns the TupleState and whether the snapshot was successfully created.
// On failure, the update proceeds in degraded mode (no rollback capability).
func (m *AppManager) snapshotTupleBeforeUpdate(
	ctx context.Context, state *FilesystemStateManager,
	appInst *AppInstance, primary string,
) (tupleState *TupleState, snapshotOK bool) {
	instanceID := appInst.InstanceID

	// Load or create TupleState.
	ts, err := state.LoadTupleState(instanceID)
	if err != nil {
		log.Printf("WARN: update %s: load tuple state: %v", instanceID, err)
		return nil, false
	}
	if ts == nil {
		ts = &TupleState{
			InstanceID:    instanceID,
			NextGenNumber: 1,
		}
	}

	// Always create a fresh snapshot — even if a previous snapshot exists for the same rootfs state.
	// The data volume may have accumulated writes since the last snapshot, so reusing an old one
	// would lose data on rollback (e.g., update retry after partial failure).

	// Allocate generation ID.
	genID := ts.AllocateGenerationID()
	// Extract generation number from ID for LV naming.
	genNumber := ts.NextGenNumber - 1

	// Capture current rootfs state.
	rootfsVolIDs := make(map[string]string)
	if appInst.ActiveRootfs != nil {
		for k, v := range appInst.ActiveRootfs {
			rootfsVolIDs[k] = v
		}
	} else if primary != "" {
		// Legacy fallback.
		rootfsVolIDs[primary] = persistence.ServiceRootfsVolumeID(instanceID, primary)
	}

	// Snapshot data volume.
	snapshotLVName := DataSnapshotLVName(instanceID, genNumber)
	dataSnapshotName := ""
	if snapshotter, ok := m.currentVolumeManager().(dataVolumeSnapshotter); ok {
		if snapErr := snapshotter.SnapshotDataVolume(ctx, instanceID, snapshotLVName); snapErr != nil {
			log.Printf("WARN: update %s: data snapshot failed: %v", instanceID, snapErr)
		} else {
			dataSnapshotName = snapshotLVName
			snapshotOK = true
			log.Printf("INFO: update %s: data snapshot created: %s", instanceID, snapshotLVName)
		}
	} else {
		log.Printf("WARN: update %s: volume manager does not support snapshots", instanceID)
	}

	// Only create a rollback-capable snapshot generation if the data snapshot succeeded.
	// Without a data snapshot, rootfs-only rollback would run old rootfs against new data.
	if dataSnapshotName == "" {
		log.Printf("WARN: update %s: no data snapshot — update proceeds without rollback capability", instanceID)
		return ts, false
	}

	// Deprecate existing active and snapshot generations.
	// Only the new snapshot (with data snapshot LV) is a valid rollback target.
	now := time.Now()
	for i := range ts.Generations {
		g := &ts.Generations[i]
		if g.Status == TupleStatusActive || g.Status == TupleStatusSnapshot {
			g.Status = TupleStatusDeprecated
			g.DeprecatedAt = &now
		}
	}

	// Append new snapshot generation.
	ts.Generations = append(ts.Generations, TupleGeneration{
		ID:           genID,
		RootfsVolIDs: rootfsVolIDs,
		DataSnapshot: dataSnapshotName,
		CreatedAt:    time.Now(),
		Status:       TupleStatusSnapshot,
	})

	// Persist.
	if storeErr := state.StoreTupleState(instanceID, ts); storeErr != nil {
		log.Printf("WARN: update %s: persist tuple state: %v", instanceID, storeErr)
		// Clean up the orphaned snapshot LV since it won't be tracked in metadata.
		if dataSnapshotName != "" {
			if cleanupSnap, ok := m.currentVolumeManager().(dataVolumeSnapshotter); ok {
				if destroyErr := cleanupSnap.DestroyDataSnapshot(ctx, dataSnapshotName); destroyErr != nil {
					log.Printf("WARN: update %s: cleanup orphaned snapshot %s: %v", instanceID, dataSnapshotName, destroyErr)
				}
			}
		}
		snapshotOK = false
	}

	return ts, snapshotOK
}

// recordPostUpdateGeneration records the new active generation after a successful update.
func (m *AppManager) recordPostUpdateGeneration(state *FilesystemStateManager, appInst *AppInstance, ts *TupleState) {
	genID := ts.AllocateGenerationID()

	rootfsVolIDs := make(map[string]string)
	if appInst.ActiveRootfs != nil {
		for k, v := range appInst.ActiveRootfs {
			rootfsVolIDs[k] = v
		}
	}

	ts.Generations = append(ts.Generations, TupleGeneration{
		ID:           genID,
		RootfsVolIDs: rootfsVolIDs,
		CreatedAt:    time.Now(),
		Status:       TupleStatusActive,
	})
	ts.CurrentGeneration = genID

	if err := state.StoreTupleState(appInst.InstanceID, ts); err != nil {
		log.Printf("WARN: update %s: persist post-update generation: %v", appInst.InstanceID, err)
	}
}

// mapsEqual compares two string maps for equality.
func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// RollbackToSnapshot rolls back an app to its latest snapshot generation (RFC 20260302 Phase 3).
// This is the exported entry point — acquires reconcileMu.
func (m *AppManager) RollbackToSnapshot(ctx context.Context, instanceID string) error {
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()

	if err := m.ensureUnlocked(); err != nil {
		return err
	}
	if err := m.ensureKernelLeader(); err != nil {
		return err
	}
	state, err := m.ensureStateManager()
	if err != nil {
		return err
	}
	appInst, exists := state.GetApp(instanceID)
	if !exists {
		return fmt.Errorf("app instance not found: %s", instanceID)
	}
	return m.rollbackToSnapshotLocked(ctx, state, appInst)
}

// HasSnapshotAvailable returns whether a rollback snapshot exists for the given app instance.
func (m *AppManager) HasSnapshotAvailable(ctx context.Context, instanceID string) bool {
	state, err := m.ensureStateManager()
	if err != nil {
		return false
	}
	ts, err := state.LoadTupleState(instanceID)
	if err != nil || ts == nil {
		return false
	}
	return ts.LatestSnapshot() != nil
}

// rollbackToSnapshotLocked performs the rollback. Caller holds reconcileMu.
func (m *AppManager) rollbackToSnapshotLocked(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance) (err error) {
	instanceID := appInst.InstanceID
	m.emitProgress(ctx, taskTypeRollbackApp, instanceID, taskPhaseStopping, 0, "Stopping containers", false, nil)
	defer func() {
		if err != nil {
			m.emitProgress(ctx, taskTypeRollbackApp, instanceID, taskPhaseComplete, 100, "Rollback failed", true, err)
		} else {
			m.emitProgress(ctx, taskTypeRollbackApp, instanceID, taskPhaseComplete, 100, "Rollback complete", true, nil)
		}
	}()

	ts, err := state.LoadTupleState(instanceID)
	if err != nil {
		return fmt.Errorf("load tuple state: %w", err)
	}
	if ts == nil {
		return fmt.Errorf("no tuple state for %s: rollback not available", instanceID)
	}
	snap := ts.LatestSnapshot()
	if snap == nil {
		return fmt.Errorf("no snapshot available for rollback")
	}

	curDef, err := state.GetAppDefinition(instanceID)
	if err != nil {
		return fmt.Errorf("read app definition: %w", err)
	}
	mode := piccoloModeFromExtensions(curDef.Extensions)
	layout, err := m.ensureAppVolumeLayout(ctx, instanceID)
	if err != nil {
		return err
	}
	runtime, err := m.podmanRuntimeForApp(instanceID, layout, mode)
	if err != nil {
		return err
	}

	// 1. Stop ALL containers.
	if err := m.stopContainersForMultiApp(ctx, appInst, curDef, runtime); err != nil {
		log.Printf("WARN: rollback %s: stop containers: %v", instanceID, err)
	}

	m.emitProgress(ctx, taskTypeRollbackApp, instanceID, taskPhaseSnapshotting, 30, "Restoring data volume", false, nil)

	// 2. Rollback data volume if snapshot has one.
	dataVolumeOK := true
	snapshotCanPromote := true // false only in partial-rename case
	if snap.DataSnapshot != "" {
		rollbacker, ok := m.currentVolumeManager().(dataVolumeRollbacker)
		if !ok {
			return fmt.Errorf("volume manager does not support rollback")
		}
		// Compute failed LV name from the active generation.
		activeGen := ts.ActiveGeneration()
		failedGenNumber := ts.NextGenNumber - 1
		if activeGen != nil {
			if n, _ := fmt.Sscanf(activeGen.ID, "gen-%d", &failedGenNumber); n != 1 {
				log.Printf("WARN: rollback %s: could not parse gen number from %q, using fallback %d",
					instanceID, activeGen.ID, failedGenNumber)
			}
		}
		failedLVName := FailedDataLVName(instanceID, failedGenNumber)

		renamesCommitted, snapshotPromoted, rollbackErr := rollbacker.RollbackDataVolume(ctx, instanceID, snap.DataSnapshot, failedLVName)
		if rollbackErr != nil && !renamesCommitted {
			// Pre-swap failure (detach or rename). No LV state change — safe to abort.
			// Containers were stopped in step 1 but no LV state changed, so the reconciler
			// will detect stopped containers and restart them on the next pass.
			log.Printf("ERROR: rollback %s: data volume rollback failed (no LV change): %v", instanceID, rollbackErr)
			return fmt.Errorf("data volume rollback: %w", rollbackErr)
		}
		if rollbackErr != nil {
			log.Printf("ERROR: rollback %s: LV rename(s) committed but incomplete: %v", instanceID, rollbackErr)
			dataVolumeOK = false
		}
		if !snapshotPromoted {
			// Partial rename: active→failed succeeded but snapshot→active failed.
			// Snapshot LV still exists under its original name — do NOT mark as promoted.
			snapshotCanPromote = false
		}

		// Record failed LV name for GC tracking.
		if renamesCommitted {
			if activeGen != nil {
				activeGen.FailedLVName = failedLVName
			} else {
				// No active generation (update failed before recordPostUpdateGeneration).
				// Create a tracking entry so GC can clean the orphaned LV.
				failedNow := time.Now()
				ts.Generations = append(ts.Generations, TupleGeneration{
					ID:           fmt.Sprintf("gen-failed-%d", failedGenNumber),
					Status:       TupleStatusFailed,
					FailedLVName: failedLVName,
					FailedAt:     &failedNow,
					CreatedAt:    failedNow,
				})
			}
		}
	}

	// Re-resolve snap pointer — the append above may have reallocated the slice.
	snap = ts.LatestSnapshot()
	if snap == nil {
		// Should not happen — snapshot was validated above. Defensive check.
		return fmt.Errorf("rollback %s: snapshot generation lost after slice mutation", instanceID)
	}

	// 3. Update generation statuses EARLY — LV renames are committed at this point
	// and tuple state must reflect reality even if container creation is skipped.
	if active := ts.ActiveGeneration(); active != nil {
		active.Status = TupleStatusFailed
		failedNow := time.Now()
		active.FailedAt = &failedNow
	}
	if snapshotCanPromote {
		snap.Status = TupleStatusActive
		snap.DataSnapshot = "" // promoted — no longer a snapshot LV
		ts.CurrentGeneration = snap.ID
	}

	// 4. Swap ActiveRootfs to snapshot generation's rootfs.
	appInst.ActiveRootfs = make(map[string]string, len(snap.RootfsVolIDs))
	for k, v := range snap.RootfsVolIDs {
		appInst.ActiveRootfs[k] = v
	}
	appInst.UpdatedAt = time.Now()

	// Restore pre-update definition (best-effort)
	if prevDef, defErr := state.GetPreviousAppDefinition(instanceID); defErr == nil {
		appInst.Definition = prevDef
		curDef = prevDef // ensure container recreation uses restored definition
	} else {
		log.Printf("WARN: rollback %s: no previous definition to restore: %v", instanceID, defErr)
	}

	// 5. Persist state BEFORE container creation — ensures metadata matches LV reality
	// even if subsequent steps fail.
	if err := state.StoreTupleState(instanceID, ts); err != nil {
		log.Printf("WARN: rollback %s: persist tuple state: %v", instanceID, err)
	}
	if err := state.StoreApp(appInst); err != nil {
		m.setObservedStatus(instanceID, StatusError)
		return fmt.Errorf("store app state during rollback: %w", err)
	}

	// 6. If data volume attach failed, remove old containers and clear container state.
	// Reconciler will retry mount + container recreation using updated ActiveRootfs.
	if !dataVolumeOK {
		if err := m.removeContainersForMultiApp(ctx, appInst, curDef, runtime); err != nil {
			log.Printf("WARN: rollback %s: remove containers after attach failure: %v", instanceID, err)
		}
		appInst.NetworkAnchorID = ""
		appInst.Containers = nil
		_ = state.StoreAppMetadata(appInst)
		m.setObservedStatus(instanceID, StatusError)
		return fmt.Errorf("rollback %s: data volume not mounted (LV renames committed, reconciler will retry)", instanceID)
	}

	// 7. Capture endpoints for port reuse (keep service registry intact for proxies).
	endpoints, _ := m.serviceManager.GetByApp(instanceID)

	// 8. Remove ALL containers (but preserve service registry — proxies stay running).
	if err := m.removeContainersForMultiApp(ctx, appInst, curDef, runtime); err != nil {
		log.Printf("WARN: rollback %s: remove containers: %v", instanceID, err)
	}

	// If no endpoints exist (e.g., after error escalation), allocate fresh ones.
	if len(endpoints) == 0 {
		var allocErr error
		endpoints, allocErr = m.serviceManager.AllocateForApp(instanceID, curDef.Listeners)
		if allocErr != nil {
			return fmt.Errorf("rollback %s: allocate endpoints: %w", instanceID, allocErr)
		}
	}
	// Restore OIDC authorize paths from the rolled-back definition on both paths.
	m.configureOIDCAuthorizePaths(instanceID, curDef)

	m.emitProgress(ctx, taskTypeRollbackApp, instanceID, taskPhaseRecreatingContainer, 60, "Recreating containers", false, nil)

	// 9. Attach snapshot rootfs and recreate containers.
	rootfs := m.currentRootfsManager()
	var prebuiltRootfs map[string]*rootfsMountInfo
	if rootfs != nil {
		prebuiltRootfs, err = m.ensureAllServiceRootfsAttached(ctx, instanceID, mode, curDef, appInst)
		if err != nil {
			// Rootfs attach failed — cannot create containers with correct snapshot rootfs.
			// State is already persisted and consistent. Reconciler will retry.
			m.setObservedStatus(instanceID, StatusError)
			return fmt.Errorf("rollback %s: attach snapshot rootfs: %w", instanceID, err)
		}
	}
	result, err := m.installContainerGroup(ctx, curDef, instanceID, layout, runtime, endpoints, prebuiltRootfs)
	if err != nil {
		m.setObservedStatus(instanceID, StatusError)
		return fmt.Errorf("recreate containers after rollback: %w", err)
	}

	// 10. Update container IDs and persist final state.
	appInst.NetworkAnchorID = result.NetworkAnchorID
	appInst.Containers = result.Containers
	appInst.PrimaryService = result.PrimaryService
	appInst.UpdatedAt = time.Now()

	if err := state.StoreApp(appInst); err != nil {
		m.setObservedStatus(instanceID, StatusError)
		return fmt.Errorf("store app after rollback: %w", err)
	}

	m.setObservedStatus(instanceID, StatusRunning)
	log.Printf("INFO: rollback %s: rolled back to generation %s", instanceID, snap.ID)
	return nil
}

// UpdateListeners updates an app instance's listeners and recreates the container if necessary.
// ctx must NOT be derived from an HTTP request context — callers must use a server-scoped
// context so that the internal rollback closure (which captures ctx) survives connection drops.
func (m *AppManager) UpdateListeners(ctx context.Context, instanceID string, listeners []api.AppListener) (*api.AppDefinition, error) {
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()
	return m.updateListenersLocked(ctx, instanceID, listeners)
}

func (m *AppManager) updateListenersLocked(ctx context.Context, instanceID string, listeners []api.AppListener) (def *api.AppDefinition, err error) {
	m.emitProgress(ctx, taskTypeUpdateListeners, instanceID, taskPhaseValidating, 0, "Validating listener configuration", false, nil)
	defer func() {
		if err != nil {
			m.emitProgress(ctx, taskTypeUpdateListeners, instanceID, taskPhaseComplete, 100, "Update failed", true, err)
			return
		}
		m.emitProgress(ctx, taskTypeUpdateListeners, instanceID, taskPhaseComplete, 100, "Listeners updated", true, nil)
	}()

	if err := m.ensureUnlocked(); err != nil {
		return nil, err
	}
	if err := m.ensureKernelLeader(); err != nil {
		return nil, err
	}
	state, err := m.ensureStateManager()
	if err != nil {
		return nil, err
	}
	appInst, exists := state.GetApp(instanceID)
	if !exists {
		return nil, fmt.Errorf("app instance not found: %s", instanceID)
	}

	layout, err := m.ensureAppVolumeLayout(ctx, instanceID)
	if err != nil {
		return nil, err
	}

	// Load current app definition
	curDef, err := state.GetAppDefinition(instanceID)
	if err != nil {
		return nil, fmt.Errorf("failed to read current app.yaml: %w", err)
	}

	// Prepare new definition
	newDef := *curDef
	newDef.Listeners = listeners

	// Check mode - listener updates are only supported for workspace apps
	mode := piccoloModeFromExtensions(curDef.Extensions)
	if mode != ModeWorkspace {
		return nil, fmt.Errorf("listener updates are only supported for workspace mode apps")
	}

	runtime, err := m.podmanRuntimeForApp(instanceID, layout, mode)
	if err != nil {
		return nil, err
	}

	// Auto-designate a primary listener if none is marked.
	// The API caller doesn't set Primary (it's an internal concept from YAML install).
	// For workspace apps editing listeners, pick the first eligible listener as primary.
	// Primary listeners must support host-based routing (not flow:tls or protocol:raw).
	if len(newDef.Listeners) > 0 {
		hasPrimary := false
		for _, l := range newDef.Listeners {
			if l.Primary {
				hasPrimary = true
				break
			}
		}
		if !hasPrimary {
			for i, l := range newDef.Listeners {
				if l.Flow != api.FlowTLS && l.Protocol != api.ListenerProtocolRaw {
					newDef.Listeners[i].Primary = true
					break
				}
			}
		}
	}

	// Validate new definition
	if err := ValidateAppDefinition(&newDef); err != nil {
		return nil, fmt.Errorf("invalid listener configuration: %w", err)
	}

	// Backup current definition
	if err := state.BackupCurrentAppDefinition(instanceID); err != nil {
		return nil, fmt.Errorf("backup app.yaml: %w", err)
	}

	// Reconcile services
	m.emitProgress(ctx, taskTypeUpdateListeners, instanceID, taskPhaseReconcilingServices, 30, "Reconciling services", false, nil)
	result, containerChange, err := m.serviceManager.Reconcile(instanceID, listeners)
	if err != nil {
		return nil, fmt.Errorf("failed to reconcile services: %w", err)
	}

	needsRecreation := containerChange || len(result.Added) > 0 || len(result.Removed) > 0

	if needsRecreation {
		// With block-native rootfs, container recreation is always safe — the rootfs
		// persists independently of the container, so we can simply
		// stop/remove/recreate the container wrapper without data loss.
		m.emitProgress(ctx, taskTypeUpdateListeners, instanceID, taskPhaseRecreatingContainer, 50, "Recreating containers", false, nil)

		// Stop and remove ALL containers (services + anchor).
		if stopErr := m.stopContainersForMultiApp(ctx, appInst, curDef, runtime); stopErr != nil {
			log.Printf("WARN: update listeners %s: stop containers: %v", instanceID, stopErr)
		}
		if rmErr := m.removeContainersForMultiApp(ctx, appInst, curDef, runtime); rmErr != nil {
			log.Printf("WARN: update listeners %s: remove containers: %v", instanceID, rmErr)
		}

		// rollbackContainers restores old ports and recreates containers with the old definition.
		rollbackContainers := func(cause error) (*api.AppDefinition, error) {
			log.Printf("WARN: update listeners %s: rolling back after: %v", instanceID, cause)
			rbResult, _, rbErr := m.serviceManager.Reconcile(instanceID, curDef.Listeners)
			if rbErr != nil {
				log.Printf("ERROR: update listeners %s: port rollback failed: %v", instanceID, rbErr)
				m.setObservedStatus(instanceID, StatusError)
				_ = state.StoreApp(appInst)
				return nil, fmt.Errorf("update failed: %w; rollback failed (ports): %v", cause, rbErr)
			}
			rbRootfs, rbRootfsErr := m.ensureAllServiceRootfsAttached(ctx, instanceID, mode, curDef, appInst)
			if rbRootfsErr != nil {
				m.setObservedStatus(instanceID, StatusError)
				_ = state.StoreApp(appInst)
				return nil, fmt.Errorf("update failed: %w; rollback failed (rootfs): %v", cause, rbRootfsErr)
			}
			rbInst, rbInstErr := m.installContainerGroup(ctx, curDef, instanceID, layout, runtime, rbResult.Endpoints, rbRootfs)
			if rbInstErr != nil {
				m.setObservedStatus(instanceID, StatusError)
				_ = state.StoreApp(appInst)
				return nil, fmt.Errorf("update failed: %w; rollback failed (install): %v", cause, rbInstErr)
			}
			appInst.Definition = curDef
			appInst.PrimaryService = rbInst.PrimaryService
			appInst.NetworkAnchorID = rbInst.NetworkAnchorID
			appInst.Containers = rbInst.Containers
			m.setObservedStatus(instanceID, StatusRunning)
			if saveErr := state.StoreApp(appInst); saveErr != nil {
				log.Printf("WARN: update listeners %s: failed to save rollback state: %v", instanceID, saveErr)
			}
			return nil, fmt.Errorf("update failed: %w (rolled back to previous state)", cause)
		}

		// Attach all service rootfs volumes (idempotent — returns cached handle if mounted).
		prebuiltRootfs, rErr := m.ensureAllServiceRootfsAttached(ctx, instanceID, mode, &newDef, appInst)
		if rErr != nil {
			return rollbackContainers(fmt.Errorf("attach rootfs: %w", rErr))
		}

		// Recreate the entire container group (anchor + services) with updated endpoints.
		m.emitProgress(ctx, taskTypeUpdateListeners, instanceID, taskPhaseStarting, 70, "Starting containers", false, nil)
		newInst, installErr := m.installContainerGroup(ctx, &newDef, instanceID, layout, runtime, result.Endpoints, prebuiltRootfs)
		if installErr != nil {
			return rollbackContainers(fmt.Errorf("recreate containers: %w", installErr))
		}

		// Update instance with new container group state.
		appInst.PrimaryService = newInst.PrimaryService
		appInst.NetworkAnchorID = newInst.NetworkAnchorID
		appInst.Containers = newInst.Containers
		m.setObservedStatus(instanceID, StatusRunning)
	}

	m.emitProgress(ctx, taskTypeUpdateListeners, instanceID, taskPhaseFinalizing, 90, "Saving configuration", false, nil)
	appInst.UpdatedAt = time.Now()
	appInst.Definition = &newDef
	if err := state.StoreApp(appInst); err != nil {
		// Cleanup containers created by installContainerGroup to prevent orphans.
		if needsRecreation {
			if rmErr := m.removeContainersForMultiApp(ctx, appInst, &newDef, runtime); rmErr != nil {
				log.Printf("WARN: update listeners %s: cleanup after persist failure: %v", instanceID, rmErr)
			}
			m.setObservedStatus(instanceID, StatusError)
		}
		return nil, fmt.Errorf("store app: %w", err)
	}

	return &newDef, nil
}

// Revert is a no-op stub. Legacy revert is superseded by tuple-based rollback
// (RollbackToSnapshot). Returns an error unconditionally.
func (m *AppManager) Revert(ctx context.Context, instanceID string) error {
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()
	return m.revertLocked(ctx, instanceID)
}

func (m *AppManager) revertLocked(_ context.Context, _ string) error {
	return fmt.Errorf("revert not supported: use rollback for service apps")
}

// Logs fetches recent container logs for an app instance by instanceID.
func (m *AppManager) Logs(ctx context.Context, instanceID string, lines int) ([]string, error) {
	return m.LogsForService(ctx, instanceID, "", lines)
}

// LogsStream returns a follow-stream of container logs for an app instance by instanceID.
func (m *AppManager) LogsStream(ctx context.Context, instanceID string, lines int, timestamps bool) (io.ReadCloser, error) {
	return m.LogsStreamForService(ctx, instanceID, "", lines, timestamps)
}

// LogsForService fetches recent container logs for a specific service container in an app instance.
// If service is empty, defaults to the primary service.
func (m *AppManager) LogsForService(ctx context.Context, instanceID, service string, lines int) ([]string, error) {
	// Best-effort guard: uninstall may start between this check and the podman call below.
	if m.getObservedStatus(instanceID) == StatusUninstalling {
		return nil, ErrAppUninstalling
	}
	state, err := m.ensureStateManager()
	if err != nil {
		return nil, err
	}
	appInst, exists := state.GetApp(instanceID)
	if !exists {
		return nil, fmt.Errorf("app instance not found: %s", instanceID)
	}
	if lines <= 0 {
		lines = 200
	}
	layout, err := m.ensureAppVolumeLayout(ctx, instanceID)
	if err != nil {
		return nil, err
	}

	def := appInst.Definition
	if def == nil || def.Services == nil {
		return nil, fmt.Errorf("app %s has no valid definition", instanceID)
	}

	mode := piccoloModeFromExtensions(def.Extensions)
	runtime, err := m.podmanRuntimeForApp(instanceID, layout, mode)
	if err != nil {
		return nil, err
	}

	primary := primaryServiceFor(def, appInst)
	target := strings.TrimSpace(service)
	if target == "" {
		target = primary
	}
	if target == networkAnchorServiceName {
		return nil, fmt.Errorf("invalid service name")
	}
	if _, ok := def.Services[target]; !ok {
		return nil, fmt.Errorf("unknown service '%s'", target)
	}

	cid := strings.TrimSpace(appInst.Containers[target])
	if cid == "" {
		name := containerNameForService(instanceID, target, primary)
		if id, err := m.containerManager.ResolveContainerIDByName(ctx, runtime, name); err == nil {
			cid = id
			if appInst.Containers == nil {
				appInst.Containers = make(map[string]string)
			}
			appInst.Containers[target] = id
			if err := state.StoreAppMetadata(appInst); err != nil {
				log.Printf("WARN: LogsForService %s: failed to persist resolved container ID: %v", instanceID, err)
			}
		}
	}
	if cid == "" {
		return nil, fmt.Errorf("container not found for service '%s'", target)
	}
	return m.containerManager.Logs(ctx, runtime, cid, lines)
}

// LogsStreamForService returns a follow-stream of container logs for a specific service container in an app instance.
// If service is empty, defaults to the primary service.
func (m *AppManager) LogsStreamForService(ctx context.Context, instanceID, service string, lines int, timestamps bool) (io.ReadCloser, error) {
	// Best-effort guard: uninstall may start between this check and the podman call below.
	if m.getObservedStatus(instanceID) == StatusUninstalling {
		return nil, ErrAppUninstalling
	}
	state, err := m.ensureStateManager()
	if err != nil {
		return nil, err
	}
	appInst, exists := state.GetApp(instanceID)
	if !exists {
		return nil, fmt.Errorf("app instance not found: %s", instanceID)
	}
	if lines <= 0 {
		lines = 200
	}
	layout, err := m.ensureAppVolumeLayout(ctx, instanceID)
	if err != nil {
		return nil, err
	}

	def := appInst.Definition
	if def == nil || def.Services == nil {
		return nil, fmt.Errorf("app %s has no valid definition", instanceID)
	}

	mode := piccoloModeFromExtensions(def.Extensions)
	runtime, err := m.podmanRuntimeForApp(instanceID, layout, mode)
	if err != nil {
		return nil, err
	}

	primary := primaryServiceFor(def, appInst)
	target := strings.TrimSpace(service)
	if target == "" {
		target = primary
	}
	if target == networkAnchorServiceName {
		return nil, fmt.Errorf("invalid service name")
	}
	if _, ok := def.Services[target]; !ok {
		return nil, fmt.Errorf("unknown service '%s'", target)
	}

	cid := strings.TrimSpace(appInst.Containers[target])
	if cid == "" {
		name := containerNameForService(instanceID, target, primary)
		if id, err := m.containerManager.ResolveContainerIDByName(ctx, runtime, name); err == nil {
			cid = id
			if appInst.Containers == nil {
				appInst.Containers = make(map[string]string)
			}
			appInst.Containers[target] = id
			if err := state.StoreAppMetadata(appInst); err != nil {
				log.Printf("WARN: LogsStreamForService %s: failed to persist resolved container ID: %v", instanceID, err)
			}
		}
	}
	if cid == "" {
		return nil, fmt.Errorf("container not found for service '%s'", target)
	}
	return m.containerManager.LogsStream(ctx, runtime, cid, lines, timestamps)
}

func (m *AppManager) applyOIDCClientInjection(spec *container.ContainerCreateSpec, oidcClient *api.ServiceOIDCClient) {
	if spec == nil || oidcClient == nil {
		return
	}

	// Inject auth environment variables (service-scoped).
	if len(oidcClient.Env) > 0 {
		if spec.Environment == nil {
			spec.Environment = make(map[string]string)
		}
		for k, v := range oidcClient.Env {
			spec.Environment[k] = v
		}
	}

	// Mount Piccolo Internal CA for OIDC back-channel trust.
	m.stateMu.RLock()
	caHostPath := m.internalCAPath
	oidcHost := m.oidcHostname
	m.stateMu.RUnlock()
	if caHostPath == "" {
		return
	}

	containerPath := strings.TrimSpace(oidcClient.CAMountPath)
	if containerPath != "" {
		spec.CAMounts = append(spec.CAMounts, container.CAMount{
			HostPath:      caHostPath,
			ContainerPath: containerPath,
		})
	}

	// Only add extra hosts if the container owns its network namespace.
	// Containers using NetworkMode "container:<id>" share the network namespace
	// and podman doesn't allow extra hosts in that case.
	if !strings.HasPrefix(spec.NetworkMode, "container:") {
		if entries, err := container.HostGatewayEntries(oidcHost); err == nil {
			spec.ExtraHosts = append(spec.ExtraHosts, entries...)
		} else {
			log.Printf("WARN: failed to resolve host gateway for OIDC hostname %s: %v", oidcHost, err)
		}
	}
}

// primarySvcInit returns the Init field of the primary service for the given definition.
// Returns "" if no primary service or services map is nil.
func primarySvcInit(def *api.AppDefinition) string {
	if def == nil || def.Services == nil {
		return ""
	}
	primary := primaryServiceFor(def, nil)
	if svc, ok := def.Services[primary]; ok {
		return svc.Init
	}
	return ""
}

// buildOriginalCommand constructs the original container command from image config.
// The entrypoint and cmd are combined into a single command slice that will be
// passed to boot.sh, which will execute it as the primary process.
func buildOriginalCommand(imgConfig *container.ImageConfig) []string {
	var cmd []string
	cmd = append(cmd, imgConfig.Entrypoint...)
	cmd = append(cmd, imgConfig.Cmd...)
	if len(cmd) == 0 {
		// Fallback for images without explicit entrypoint/cmd
		cmd = []string{"/bin/sh"}
	}
	return cmd
}

// SearchImages searches for container images in registries.
// This uses system defaults (no app-specific storage) since image search
// doesn't require access to app-specific container storage.
func (m *AppManager) SearchImages(ctx context.Context, query string, limit int) ([]container.ImageSearchResult, error) {
	// Use empty runtime to use system defaults
	runtime := container.PodmanRuntime{}
	return m.containerManager.SearchRegistry(ctx, runtime, query, limit)
}

// ExecShellCmd returns an exec.Cmd for running a shell inside the container for the given app instance.
// The caller is responsible for starting the command and managing its lifecycle (e.g., with PTY).
func (m *AppManager) ExecShellCmd(ctx context.Context, instanceID string) (*exec.Cmd, error) {
	return m.ExecShellCmdForService(ctx, instanceID, "")
}

// ExecShellCmdForService returns an exec.Cmd for running a shell inside a specific service container.
// If service is empty, defaults to the primary service.
func (m *AppManager) ExecShellCmdForService(ctx context.Context, instanceID, service string) (*exec.Cmd, error) {
	state, err := m.ensureStateManager()
	if err != nil {
		return nil, err
	}
	appInst, exists := state.GetApp(instanceID)
	if !exists {
		return nil, fmt.Errorf("app instance not found: %s", instanceID)
	}
	if observed := m.getObservedStatus(instanceID); observed != StatusRunning {
		return nil, fmt.Errorf("app %s is not running (status: %s)", instanceID, observed)
	}

	layout, err := m.ensureAppVolumeLayout(ctx, instanceID)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve volume layout: %w", err)
	}

	def := appInst.Definition
	if def == nil || def.Services == nil {
		return nil, fmt.Errorf("app %s has no valid definition", instanceID)
	}

	mode := piccoloModeFromExtensions(def.Extensions)
	runtime, err := m.podmanRuntimeForApp(instanceID, layout, mode)
	if err != nil {
		return nil, fmt.Errorf("failed to create podman runtime: %w", err)
	}

	primary := primaryServiceFor(def, appInst)
	target := strings.TrimSpace(service)
	if target == "" {
		target = primary
	}
	if target == networkAnchorServiceName {
		return nil, fmt.Errorf("invalid service name")
	}
	if _, ok := def.Services[target]; !ok {
		return nil, fmt.Errorf("unknown service '%s'", target)
	}

	cid := strings.TrimSpace(appInst.Containers[target])
	if cid == "" {
		name := containerNameForService(instanceID, target, primary)
		if id, err := m.containerManager.ResolveContainerIDByName(ctx, runtime, name); err == nil {
			cid = id
			if appInst.Containers == nil {
				appInst.Containers = make(map[string]string)
			}
			appInst.Containers[target] = id
			if err := state.StoreAppMetadata(appInst); err != nil {
				log.Printf("WARN: ShellForService %s: failed to persist resolved container ID: %v", instanceID, err)
			}
		}
	}
	if cid == "" {
		return nil, fmt.Errorf("container not found for service '%s'", target)
	}
	return m.containerManager.ExecShellCmd(runtime, cid)
}
