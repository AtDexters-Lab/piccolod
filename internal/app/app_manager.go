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
	"slices"
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
	"piccolod/internal/resources/pressure"
	"piccolod/internal/router"
	"piccolod/internal/services"
	"piccolod/internal/state/paths"
)

// AppManager manages application lifecycle with filesystem-based state storage
type AppManager struct {
	containerManager      ContainerManager
	stateManager          *FilesystemStateManager
	stateBaseDir          string
	stateInitMu           sync.Mutex
	serviceManager        *services.ServiceManager
	routeRegistrar        router.Registrar
	progressReporter      events.ProgressReporter
	eventBus              *events.Bus
	eventsMu              sync.Mutex
	eventCancel           context.CancelFunc
	eventSubCancels       []func()
	eventsWG              sync.WaitGroup
	reconcileMu           sync.Mutex
	reconcileCancel       context.CancelFunc
	reconcileWG           sync.WaitGroup
	stateMu               sync.RWMutex
	leadershipMu          sync.RWMutex
	leadershipState       map[string]cluster.Role
	lockReader            LockStateReader
	volumeManager         persistence.VolumeManager
	restoreMu             sync.Mutex
	pendingRestore        bool
	lockOverrideMu        sync.RWMutex
	lockOverride          *bool
	mountVerifier         func(string) error
	runtimeReadinessProbe func(context.Context, []services.ServiceEndpoint, time.Duration) error
	imageDigestResolver   func(context.Context, string) (string, error)

	// In-memory observed status: derived from container state during reconciliation.
	// Published via event bus and returned in API responses. Never persisted.
	observedStatus         map[string]string
	observedStatusMessage  map[string]string // transient status message for UI context
	observedStatusMu       sync.RWMutex
	startupRecoveryMu      sync.Mutex
	startupRecovery        map[string]startupRecoveryWindow
	unknownObservationMu   sync.Mutex
	unknownObservations    map[string]unknownObservationWindow
	observationGeneration  uint64
	runtimeSentinel        func(context.Context) error
	taskPressureNormal     func() bool
	automaticSuppressionMu sync.RWMutex
	automaticSuppression   map[string]string
	quiesceFinalizeMu      sync.Mutex

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
	// userSessionQuiescer overrides PID 1 user-unit quiescence for tests.
	userSessionQuiescer func(context.Context, string) error
	// appUserDestroyer overrides final per-app identity cleanup for tests.
	appUserDestroyer func(string) error

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

	// Server-side dry-run candidates for custom manifest update. The token
	// exposed to clients is only an opaque handle; rendered YAML and generated
	// values stay process-local until apply consumes or expires the entry.
	manifestUpdateMu         sync.Mutex
	manifestUpdateCandidates map[string]*manifestUpdateCandidate

	// Server-side dry-run candidates for installed app config updates. Tokens
	// remain opaque so replacement secrets and generated values do not return
	// to the browser after dry run.
	configUpdateMu         sync.Mutex
	configUpdateCandidates map[string]*installedConfigCandidate

	capabilityIngressMu   sync.Mutex
	capabilityIngresses   map[capabilityIngressKey]*capabilityIngress
	capabilityListen      capabilityIngressListenFunc
	acceleratorDiscover   func() ([]string, error)
	acceleratorPermission func(context.Context, uint32, []string, bool) error
}

var (
	ErrLocked                        = errors.New("app manager: persistence locked")
	ErrNotLeader                     = errors.New("app manager: not leader")
	ErrVolumeUnavailable             = errors.New("app manager: persistence volume not mounted")
	ErrAppUninstalling               = errors.New("app manager: app is being uninstalled")
	ErrImageUpdateRejected           = errors.New("image update rejected")
	ErrTransitionInProgress          = errors.New("app manager: app transition in progress")
	ErrTransitionFollowUpUnavailable = errors.New("app manager: transition follow-up unavailable")
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
		containerManager:         containerManager,
		stateBaseDir:             base,
		serviceManager:           serviceManager,
		leadershipState:          make(map[string]cluster.Role),
		lockReader:               lockReader,
		mountVerifier:            defaultMountVerifier,
		runtimeReadinessProbe:    defaultRuntimeReadinessProbe,
		observedStatus:           make(map[string]string),
		observedStatusMessage:    make(map[string]string),
		startupRecovery:          make(map[string]startupRecoveryWindow),
		unknownObservations:      make(map[string]unknownObservationWindow),
		automaticSuppression:     make(map[string]string),
		oidcHostname:             "piccolo.local",
		runtimeUser:              *runtimeUser,
		syncInFlight:             make(map[string]bool),
		manifestUpdateCandidates: make(map[string]*manifestUpdateCandidate),
		configUpdateCandidates:   make(map[string]*installedConfigCandidate),
		capabilityIngresses:      make(map[capabilityIngressKey]*capabilityIngress),
	}

	return mgr, nil
}

// SetTaskPressureNormal supplies the production task-guard authority used by
// destructive persistent-unknown recovery. Tests may leave it nil, in which
// case an open admission gate is treated as Normal.
func (m *AppManager) SetTaskPressureNormal(fn func() bool) {
	m.stateMu.Lock()
	m.taskPressureNormal = fn
	m.stateMu.Unlock()
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

type taskProgressReader interface {
	Last(taskID string) (events.TaskProgressEvent, bool)
}

func (m *AppManager) inheritedTaskProgress(ctx context.Context, fallbackType string, fallbackProgress int) (string, int) {
	taskID := TaskIDFromContext(ctx)
	reporter := m.currentProgressReporter()
	reader, ok := reporter.(taskProgressReader)
	if taskID == "" || !ok {
		return fallbackType, fallbackProgress
	}
	last, ok := reader.Last(taskID)
	if !ok {
		return fallbackType, fallbackProgress
	}
	if strings.TrimSpace(last.TaskType) != "" {
		fallbackType = last.TaskType
	}
	if last.Progress > fallbackProgress {
		fallbackProgress = last.Progress
	}
	return fallbackType, fallbackProgress
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
	m.startBackground(true)
}

// StartBackgroundAfterInitial starts only periodic work. It is used after a
// serialized task-recovery pass so immediate reconcile work is not duplicated
// in detached goroutines.
func (m *AppManager) StartBackgroundAfterInitial() {
	m.startBackground(false)
}

func (m *AppManager) startBackground(runInitial bool) {
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

		if runInitial {
			m.ReconcileOnce(ctx)
		}
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
		if runInitial {
			m.ReconcileAllSlicePolicies()
		}
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

	m.startCatalogSyncLoop(ctx, runInitial)
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
	m.closeCapabilityIngresses()
}

// StopAllApps proves every app cgroup quiescent before detaching its volume.
// Cached observed status is not sufficient evidence that no process can still
// write to the mounted filesystem. Apps are processed in parallel for
// efficiency, with detach skipped for any app whose quiescence proof fails.
func (m *AppManager) StopAllApps(ctx context.Context) error {
	log.Printf("INFO: Quiescing all apps for graceful shutdown...")

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

	var errs []error
	log.Printf("INFO: Quiescing %d apps in parallel...", len(apps))

	const maxConcurrency = 4
	sem := make(chan struct{}, maxConcurrency)
	var wg sync.WaitGroup
	var errMu sync.Mutex

	for i, app := range apps {
		wg.Add(1)
		go func(idx int, appInst *AppInstance) {
			defer wg.Done()

			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				errMu.Lock()
				errs = append(errs, fmt.Errorf("app %s: context cancelled before quiesce: %w", appInst.InstanceID, ctx.Err()))
				errMu.Unlock()
				return
			}

			log.Printf("INFO: [%d/%d] Quiescing app %s...", idx+1, len(apps), appInst.InstanceID)
			if err := m.stopAppForShutdown(ctx, appInst.InstanceID); err != nil {
				log.Printf("WARN: Failed to prove app %s quiescent; leaving volume attached: %v", appInst.InstanceID, err)
				errMu.Lock()
				errs = append(errs, fmt.Errorf("quiesce %s: %w", appInst.InstanceID, err))
				errMu.Unlock()
				return
			}

			if err := m.detachAppVolume(ctx, appInst.InstanceID); err != nil {
				log.Printf("WARN: Failed to detach volume for %s: %v", appInst.InstanceID, err)
				errMu.Lock()
				errs = append(errs, fmt.Errorf("detach %s: %w", appInst.InstanceID, err))
				errMu.Unlock()
				return
			}
			log.Printf("INFO: [%d/%d] Quiesced app %s", idx+1, len(apps), appInst.InstanceID)
		}(i, app)
	}

	wg.Wait()

	log.Printf("INFO: Finished stopping all apps")

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// stopAppForShutdown stops an app's containers without updating state or
// emitting progress events. This is a simplified path for graceful shutdown.
func (m *AppManager) stopAppForShutdown(ctx context.Context, instanceID string) error {
	// Graceful shutdown already owns this quiescence boundary. Allow the
	// existing PID 1/Podman cleanup path to finish through a Warning fence;
	// the Critical hard fence still rejects every child-producing command.
	ctx = pressure.WithTransitionContinuation(ctx)

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
	if m.serviceManager != nil {
		m.serviceManager.DeactivateApp(instanceID)
	}

	def, err := stateMgr.GetAppDefinition(instanceID)
	if err != nil {
		if quiesceErr := m.quiesceAppUserSession(ctx, instanceID); quiesceErr != nil {
			return errors.Join(fmt.Errorf("failed to load app definition: %w", err), quiesceErr)
		}
		if detachErr := m.detachArtifactReferences(ctx, app.ArtifactReferences); detachErr != nil {
			return errors.Join(fmt.Errorf("failed to load app definition: %w", err), detachErr)
		}
		return fmt.Errorf("failed to load app definition after safe quiescence: %w", err)
	}

	layout, err := m.ensureAppVolumeLayout(ctx, instanceID)
	if err != nil {
		// Runtime storage may already be unavailable, but PID 1 can still prove
		// that the dedicated user cgroup is empty without consulting Podman.
		if quiesceErr := m.quiesceAppUserSession(ctx, instanceID); quiesceErr != nil {
			return errors.Join(fmt.Errorf("resolve volume layout: %w", err), quiesceErr)
		}
		m.finalizeQuiescedContainerGroup(ctx, app, def, piccoloModeFromExtensions(def.Extensions))
		return nil
	}

	// Daemon shutdown is a transition-boundary quiesce exception: it may stop
	// processes, but must not mutate app source/rootfs metadata or transition
	// records. Startup recovery remains responsible for any active transition.
	return m.quiesceContainerGroup(ctx, stateMgr, app, def, layout)
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
	svc.UseInMemoryNetworkForTest()
	return &AppManager{
		containerManager:      containerManager,
		stateBaseDir:          base,
		serviceManager:        svc,
		leadershipState:       make(map[string]cluster.Role),
		mountVerifier:         testMountVerifier,
		runtimeReadinessProbe: testRuntimeReadinessProbe,
		imageDigestResolver: func(_ context.Context, imageRef string) (string, error) {
			return imageRef + "@sha256:mockdigest", nil
		},
		observedStatus:        make(map[string]string),
		observedStatusMessage: make(map[string]string),
		startupRecovery:       make(map[string]startupRecoveryWindow),
		unknownObservations:   make(map[string]unknownObservationWindow),
		automaticSuppression:  make(map[string]string),
		oidcHostname:          "piccolo.local",
		runtimeUser: container.RuntimeUser{
			Credential: testCred,
			HomeDir:    testHome,
		},
		credentialResolver: func(string) (*syscall.Credential, string, error) {
			return testCred, testHome, nil
		},
		acceleratorDiscover:      func() ([]string, error) { return nil, nil },
		userSessionQuiescer:      func(context.Context, string) error { return nil },
		syncInFlight:             make(map[string]bool),
		manifestUpdateCandidates: make(map[string]*manifestUpdateCandidate),
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
		containerManager:      containerManager,
		stateBaseDir:          base,
		serviceManager:        serviceManager,
		leadershipState:       make(map[string]cluster.Role),
		lockReader:            lockReader,
		mountVerifier:         testMountVerifier,
		runtimeReadinessProbe: testRuntimeReadinessProbe,
		imageDigestResolver: func(_ context.Context, imageRef string) (string, error) {
			return imageRef + "@sha256:mockdigest", nil
		},
		observedStatus:        make(map[string]string),
		observedStatusMessage: make(map[string]string),
		startupRecovery:       make(map[string]startupRecoveryWindow),
		unknownObservations:   make(map[string]unknownObservationWindow),
		automaticSuppression:  make(map[string]string),
		oidcHostname:          "piccolo.local",
		runtimeUser: container.RuntimeUser{
			Credential: testCred,
			HomeDir:    testHome,
		},
		credentialResolver: func(string) (*syscall.Credential, string, error) {
			return testCred, testHome, nil
		},
		acceleratorDiscover:      func() ([]string, error) { return nil, nil },
		userSessionQuiescer:      func(context.Context, string) error { return nil },
		syncInFlight:             make(map[string]bool),
		manifestUpdateCandidates: make(map[string]*manifestUpdateCandidate),
	}, nil
}

// RestoreServices rebuilds service proxies for running apps based on current container port bindings.
func (m *AppManager) RestoreServices(ctx context.Context) {
	if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkLifecycle); err != nil {
		return
	}
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
		if app == nil || m.automaticRecoverySuppressed(app.InstanceID) {
			continue
		}
		m.restoreServiceForApp(ctx, state, app)
	}
}

func (m *AppManager) restoreServiceForApp(ctx context.Context, state *FilesystemStateManager, app *AppInstance) {
	releaseOwner := pressure.BeginLifecycleOwner("app:" + app.InstanceID)
	defer releaseOwner()
	if m.manifestUpdateServiceRestoreBlocked(state, app.InstanceID) {
		if m.serviceManager != nil {
			m.serviceManager.DeactivateApp(app.InstanceID)
		}
		return
	}
	if m.imageUpdateServiceRestoreBlocked(state, app.InstanceID) {
		if m.serviceManager != nil {
			m.serviceManager.DeactivateApp(app.InstanceID)
		}
		return
	}
	// Respect desired state: disabled apps should not have proxies restored.
	if !app.Enabled {
		if m.serviceManager != nil {
			m.serviceManager.DeactivateApp(app.InstanceID)
		}
		return
	}
	// Followers should not restore proxies for apps they don't lead.
	if m.LastObservedRole(cluster.ResourceForApp(app.InstanceID)) == cluster.RoleFollower {
		if m.serviceManager != nil {
			m.serviceManager.DeactivateApp(app.InstanceID)
		}
		return
	}
	def, err := state.GetAppDefinition(app.InstanceID)
	if err != nil {
		log.Printf("WARN: restore services: failed to read app definition for %s: %v", app.InstanceID, err)
		return
	}
	layout, err := m.observeAppVolumeLayout(ctx, app.InstanceID)
	if err != nil {
		log.Printf("WARN: restore services: app volume unavailable for %s: %v", app.InstanceID, err)
		m.recordUnknownObservation(app.InstanceID, err)
		return
	}
	runtime, err := m.podmanRuntimeForApp(ctx, app.InstanceID, layout, piccoloModeFromExtensions(def.Extensions), appRuntimeObserve)
	if err != nil {
		log.Printf("WARN: restore services: podman runtime unavailable for %s: %v", app.InstanceID, err)
		m.recordUnknownObservation(app.InstanceID, err)
		return
	}
	observed := m.observeContainerGroup(ctx, runtime, app, def)
	if !observed.known() {
		log.Printf("WARN: restore services: container group observation unknown for %s: %v", app.InstanceID, observed.Err)
		m.recordUnknownObservation(app.InstanceID, observed.Err)
		return
	}
	if observed.Outcome != containerGroupRunning {
		m.serviceManager.DeactivateApp(app.InstanceID)
		return
	}
	if err := m.applyContainerGroupObservation(state, app, observed); err != nil {
		log.Printf("WARN: restore services: persist observed IDs for %s: %v", app.InstanceID, err)
		return
	}
	publishCID := strings.TrimSpace(observed.Anchor.ID)
	if publishCID == "" {
		m.serviceManager.DeactivateApp(app.InstanceID)
		return
	}
	ports, err := m.containerManager.InspectPublishedPorts(ctx, runtime, publishCID)
	if err != nil {
		log.Printf("WARN: restore services: podman port inspect failed for %s: %v", app.InstanceID, err)
		m.recordUnknownObservation(app.InstanceID, err)
		return
	}
	if len(ports) == 0 {
		m.serviceManager.DeactivateApp(app.InstanceID)
		return
	}
	if _, err := m.serviceManager.RestoreFromPodmanContext(ctx, app.InstanceID, def.Listeners, ports); err != nil {
		log.Printf("WARN: restore services: failed to restore proxies for %s: %v", app.InstanceID, err)
		m.serviceManager.DeactivateApp(app.InstanceID)
		return
	}
	m.configureOIDCAuthorizePaths(app.InstanceID, def)
	m.serviceManager.SetAppContainerID(app.InstanceID, publishCID)
	m.clearUnknownObservation(app.InstanceID)
}

func (m *AppManager) imageUpdateServiceRestoreBlocked(state *FilesystemStateManager, instanceID string) bool {
	txn, err := state.LoadImageUpdateTransaction(instanceID)
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		log.Printf("WARN: restore services: image update transaction unreadable for %s: %v", instanceID, err)
		return true
	}
	switch txn.Phase {
	case imageUpdatePhaseCommitted, imageUpdatePhaseCleanupPending:
		return false
	default:
		log.Printf("INFO: restore services: skipping %s while image update recovery owns phase %s", instanceID, txn.Phase)
		return true
	}
}

func (m *AppManager) manifestUpdateServiceRestoreBlocked(state *FilesystemStateManager, instanceID string) bool {
	txn, err := state.LoadManifestUpdateTransaction(instanceID)
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		log.Printf("WARN: restore services: manifest update transaction unreadable for %s: %v", instanceID, err)
		return true
	}
	switch txn.Phase {
	case "committed", "committed_cleanup_pending":
		return false
	case "committed_metadata_pending", "access_published":
		if txn.AccessPublished {
			return false
		}
		log.Printf("INFO: restore services: skipping %s while manifest update recovery owns phase %s", instanceID, txn.Phase)
		return true
	default:
		log.Printf("INFO: restore services: skipping %s while manifest update recovery owns phase %s", instanceID, txn.Phase)
		return true
	}
}

func (m *AppManager) recoverPendingTransitionRecords(ctx context.Context, state *FilesystemStateManager) map[string]bool {
	blocked := map[string]bool{}
	for _, appInst := range state.ListApps() {
		if appInst == nil || strings.TrimSpace(appInst.InstanceID) == "" {
			continue
		}
		instanceID := appInst.InstanceID
		record, err := state.LoadTransitionRecord(instanceID)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			log.Printf("ERROR: transition recovery %s: read authoritative transition record: %v", instanceID, err)
			m.setObservedStatus(instanceID, StatusError)
			blocked[instanceID] = true
			continue
		}
		recoveryCtx, admitted := admitPendingTransitionRecovery(ctx)
		if !admitted {
			return blocked
		}
		if m.recoverPendingTransitionRecord(recoveryCtx, state, appInst, record) {
			blocked[instanceID] = true
		}
		if transitionRecoveryMustYield(recoveryCtx) {
			return blocked
		}
	}
	return blocked
}

func (m *AppManager) recoverPendingTransitionRecord(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance, record *TransitionRecord) bool {
	instanceID := appInst.InstanceID
	if record.Plan.OperationKind == TransitionOperationRuntimeRecovery {
		if err := m.recoverRuntimeRecoveryTransition(ctx, state, appInst, record); err != nil {
			log.Printf("ERROR: transition recovery %s: recover dedicated runtime quarantine: %v", instanceID, err)
			m.recordUnknownObservation(instanceID, errAppRuntimeObservationUnavailable)
			return true
		}
		if _, err := state.LoadTransitionRecord(instanceID); errors.Is(err, os.ErrNotExist) {
			m.clearUnknownObservation(instanceID)
		}
		return false
	}
	if record.Phase == TransitionPhaseCommitted {
		if err := state.ClearTransitionRecord(instanceID); err != nil {
			log.Printf("ERROR: transition recovery %s: clear committed transition record: %v", instanceID, err)
			m.setObservedStatus(instanceID, StatusError)
			return true
		}
		return false
	}
	legacy, legacyErr := loadTransitionLegacyJournals(state, instanceID)
	if legacyErr != nil {
		log.Printf("ERROR: transition recovery %s: inspect legacy journals: %v", instanceID, legacyErr)
		m.setObservedStatus(instanceID, StatusError)
		return true
	}
	if legacy.hasAny() {
		if err := m.recoverTransitionWithLegacyJournal(ctx, state, appInst, record, legacy); err != nil {
			log.Printf("ERROR: transition recovery %s: recover legacy-backed v2 transition: %v", instanceID, err)
			m.setObservedStatus(instanceID, StatusError)
			return true
		}
		return false
	}
	if record.Plan.OperationKind == TransitionOperationUpdateImage && record.Plan.SourceKind == TransitionSourceCurrentCommitted {
		if err := m.recoverV2OnlyImageUpdateTransition(ctx, state, appInst, record); err != nil {
			log.Printf("ERROR: transition recovery %s: recover v2-only image update: %v", instanceID, err)
			m.setObservedStatus(instanceID, StatusError)
			return true
		}
		return false
	}
	if record.Phase == TransitionPhasePrepared {
		log.Printf("INFO: transition recovery %s: clearing prepared transition with no legacy journal", instanceID)
		if err := state.ClearTransitionRecord(instanceID); err != nil {
			log.Printf("ERROR: transition recovery %s: clear prepared transition: %v", instanceID, err)
			m.setObservedStatus(instanceID, StatusError)
			return true
		}
		return false
	}
	log.Printf("ERROR: transition recovery %s: v2 transition %s has no legacy journal; manual repair required", instanceID, record.Phase)
	m.setObservedStatus(instanceID, StatusError)
	return true
}

type transitionLegacyJournals struct {
	manifest *ManifestUpdateTransaction
	image    *ImageUpdateTransaction
}

func (j transitionLegacyJournals) hasAny() bool {
	return j.manifest != nil || j.image != nil
}

func (j transitionLegacyJournals) count() int {
	count := 0
	if j.manifest != nil {
		count++
	}
	if j.image != nil {
		count++
	}
	return count
}

func loadTransitionLegacyJournals(state *FilesystemStateManager, instanceID string) (transitionLegacyJournals, error) {
	var out transitionLegacyJournals
	if txn, err := state.LoadManifestUpdateTransaction(instanceID); err == nil && txn != nil {
		out.manifest = txn
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return out, fmt.Errorf("manifest update transaction: %w", err)
	}
	if txn, err := state.LoadImageUpdateTransaction(instanceID); err == nil && txn != nil {
		out.image = txn
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return out, fmt.Errorf("image update transaction: %w", err)
	}
	return out, nil
}

func (m *AppManager) recoverTransitionWithLegacyJournal(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance, record *TransitionRecord, legacy transitionLegacyJournals) error {
	if appInst == nil || record == nil {
		return nil
	}
	if legacy.manifest != nil && legacy.image != nil {
		return fmt.Errorf("both manifest and image legacy journals exist; manual repair required")
	}
	switch record.Plan.OperationKind {
	case TransitionOperationUpdateImage:
		if legacy.image == nil {
			return fmt.Errorf("v2 update_image transition has non-image legacy journal")
		}
		return m.recoverOneImageUpdate(ctx, state, appInst, legacy.image)
	case TransitionOperationModifyApp, TransitionOperationEditConfig, TransitionOperationCatalogManifestReview, TransitionOperationCatalogConfigReview, TransitionOperationCatalogAutoApply:
		if legacy.manifest == nil {
			return fmt.Errorf("v2 %s transition has non-manifest legacy journal", record.Plan.OperationKind)
		}
		return m.recoverOneManifestUpdate(ctx, state, appInst, legacy.manifest)
	default:
		return fmt.Errorf("unsupported legacy-backed v2 transition operation %s", record.Plan.OperationKind)
	}
}

func (m *AppManager) RetryTransitionFollowUp(ctx context.Context, instanceID string, action TransitionActionKind) (err error) {
	defer pressure.BeginLifecycleOwner("app:" + instanceID)()
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
	if !exists || appInst == nil {
		return ErrAppNotFound
	}
	label := transitionFollowUpProgressLabel(action)
	m.emitProgress(ctx, taskTypeTransitionFollowUp, instanceID, taskPhaseValidating, 0, label, false, nil)
	defer func() {
		if err != nil {
			m.emitProgress(ctx, taskTypeTransitionFollowUp, instanceID, taskPhaseComplete, 100, label+" failed", true, err)
			return
		}
		m.emitProgress(ctx, taskTypeTransitionFollowUp, instanceID, taskPhaseComplete, 100, label+" complete", true, nil)
	}()
	return m.retryTransitionFollowUpLocked(ctx, state, appInst, action)
}

func (m *AppManager) retryTransitionFollowUpLocked(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance, action TransitionActionKind) error {
	instanceID := appInst.InstanceID
	record, err := state.LoadTransitionRecord(instanceID)
	if errors.Is(err, os.ErrNotExist) {
		return m.retryLegacyTransitionFollowUp(ctx, state, appInst, action)
	}
	if err != nil {
		return fmt.Errorf("read app transition record: %w", err)
	}
	if record == nil || record.Phase == TransitionPhaseCommitted {
		return fmt.Errorf("%w: no active transition for %s", ErrTransitionFollowUpUnavailable, action)
	}
	if !transitionFollowUpActionMatchesPhase(action, record.Phase) {
		return fmt.Errorf("%w: %s cannot consume phase %s", ErrTransitionFollowUpUnavailable, action, record.Phase)
	}
	legacy, err := loadTransitionLegacyJournals(state, instanceID)
	if err != nil {
		return err
	}
	if legacy.hasAny() {
		transitionCtx := pressure.WithTransitionContinuation(ctx)
		if err := m.recoverTransitionWithLegacyJournal(transitionCtx, state, appInst, record, legacy); err != nil {
			return err
		}
		return ensureTransitionFollowUpCompleted(state, instanceID, action)
	}
	if action == TransitionActionFinishCleanup && record.Plan.OperationKind == TransitionOperationUpdateImage && record.Plan.SourceKind == TransitionSourceCurrentCommitted {
		transitionCtx := pressure.WithTransitionContinuation(ctx)
		if err := m.recoverV2OnlyImageUpdateTransition(transitionCtx, state, appInst, record); err != nil {
			return err
		}
		return ensureTransitionFollowUpCompleted(state, instanceID, action)
	}
	return fmt.Errorf("%w: no recovery journal for %s phase %s", ErrTransitionFollowUpUnavailable, action, record.Phase)
}

func (m *AppManager) retryLegacyTransitionFollowUp(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance, action TransitionActionKind) error {
	instanceID := appInst.InstanceID
	legacy, err := loadTransitionLegacyJournals(state, instanceID)
	if err != nil {
		return err
	}
	if legacy.manifest != nil && manifestFollowUpActionMatchesPhase(action, legacy.manifest) {
		transitionCtx := pressure.WithTransitionContinuation(ctx)
		if err := m.recoverOneManifestUpdate(transitionCtx, state, appInst, legacy.manifest); err != nil {
			return err
		}
		return ensureTransitionFollowUpCompleted(state, instanceID, action)
	}
	if action == TransitionActionFinishCleanup && legacy.image != nil && imageFollowUpActionMatchesCleanup(legacy.image) {
		transitionCtx := pressure.WithTransitionContinuation(ctx)
		if err := m.recoverOneImageUpdate(transitionCtx, state, appInst, legacy.image); err != nil {
			return err
		}
		return ensureTransitionFollowUpCompleted(state, instanceID, action)
	}
	return fmt.Errorf("%w: no active %s follow-up", ErrTransitionFollowUpUnavailable, action)
}

func ensureTransitionFollowUpCompleted(state *FilesystemStateManager, instanceID string, action TransitionActionKind) error {
	pending, detail, err := transitionFollowUpStillPending(state, instanceID, action)
	if err != nil {
		return err
	}
	if pending {
		if detail != "" {
			return fmt.Errorf("%w: %s still pending: %s", ErrTransitionFollowUpUnavailable, action, detail)
		}
		return fmt.Errorf("%w: %s still pending", ErrTransitionFollowUpUnavailable, action)
	}
	return nil
}

func transitionFollowUpStillPending(state *FilesystemStateManager, instanceID string, action TransitionActionKind) (bool, string, error) {
	record, err := state.LoadTransitionRecord(instanceID)
	if err == nil && record != nil && transitionFollowUpActionMatchesPhase(action, record.Phase) {
		return true, transitionPendingDetail(string(record.Phase), record.LastError), nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, "", fmt.Errorf("read app transition record after follow-up: %w", err)
	}
	legacy, err := loadTransitionLegacyJournals(state, instanceID)
	if err != nil {
		return false, "", fmt.Errorf("read legacy transaction after follow-up: %w", err)
	}
	if legacy.manifest != nil && manifestFollowUpActionMatchesPhase(action, legacy.manifest) {
		return true, transitionPendingDetail(legacy.manifest.Phase, legacy.manifest.LastError), nil
	}
	if action == TransitionActionFinishCleanup && legacy.image != nil && imageFollowUpActionMatchesCleanup(legacy.image) {
		return true, transitionPendingDetail(legacy.image.Phase, legacy.image.LastError), nil
	}
	return false, "", nil
}

func transitionPendingDetail(phase, lastError string) string {
	phase = strings.TrimSpace(phase)
	lastError = strings.TrimSpace(lastError)
	if phase == "" {
		return lastError
	}
	if lastError == "" {
		return "phase " + phase
	}
	return fmt.Sprintf("phase %s: %s", phase, lastError)
}

func transitionFollowUpActionMatchesPhase(action TransitionActionKind, phase TransitionPhase) bool {
	switch action {
	case TransitionActionAccessRepair:
		return phase == TransitionPhasePublishingAccess
	case TransitionActionFinishCleanup:
		return phase == TransitionPhaseCommittedCleanupPending
	case TransitionActionMetadataRetry:
		return phase == TransitionPhaseCommittedMetadataPending
	default:
		return false
	}
}

func manifestFollowUpActionMatchesPhase(action TransitionActionKind, txn *ManifestUpdateTransaction) bool {
	if txn == nil {
		return false
	}
	switch action {
	case TransitionActionAccessRepair:
		return txn.Phase == "publishing_access" && !txn.AccessPublished
	case TransitionActionFinishCleanup:
		return txn.Phase == "committed" || txn.Phase == "committed_cleanup_pending"
	case TransitionActionMetadataRetry:
		return txn.Phase == "committed_metadata_pending"
	default:
		return false
	}
}

func imageFollowUpActionMatchesCleanup(txn *ImageUpdateTransaction) bool {
	if txn == nil {
		return false
	}
	return txn.Phase == imageUpdatePhaseCommitted || txn.Phase == imageUpdatePhaseCleanupPending
}

func transitionFollowUpProgressLabel(action TransitionActionKind) string {
	switch action {
	case TransitionActionAccessRepair:
		return "Access repair"
	case TransitionActionFinishCleanup:
		return "Cleanup"
	case TransitionActionMetadataRetry:
		return "Catalog metadata retry"
	default:
		return "Update follow-up"
	}
}

func transitionLegacyJournalExists(state *FilesystemStateManager, instanceID string) (bool, error) {
	legacy, err := loadTransitionLegacyJournals(state, instanceID)
	if err != nil {
		return false, err
	}
	return legacy.hasAny(), nil
}

type transitionRecoveryAdmissionKey struct{}

// transitionRecoveryAdmission owns the single Warning-pressure continuation
// available to one reconciliation pass. Normal pressure can recover every
// pending record. Under Warning, exactly one already-durable transition may
// execute its current phase; the next app/phase must wait for ordinary
// admission to reopen.
type transitionRecoveryAdmission struct {
	continuationUsed bool
}

func withTransitionRecoveryAdmission(ctx context.Context) context.Context {
	return context.WithValue(ctx, transitionRecoveryAdmissionKey{}, &transitionRecoveryAdmission{})
}

func transitionRecoveryAdmissionFrom(ctx context.Context) *transitionRecoveryAdmission {
	if ctx == nil {
		return nil
	}
	admission, _ := ctx.Value(transitionRecoveryAdmissionKey{}).(*transitionRecoveryAdmission)
	return admission
}

func admitPendingTransitionRecovery(ctx context.Context) (context.Context, bool) {
	if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkLifecycle); err == nil {
		return ctx, true
	}
	admission := transitionRecoveryAdmissionFrom(ctx)
	if admission == nil || admission.continuationUsed {
		return nil, false
	}
	continuationCtx := pressure.WithTransitionContinuation(ctx)
	// A continuation bypasses Warning only. Startup and Critical fences remain
	// authoritative and must not consume the one-transition allowance.
	if err := pressure.DefaultAdmission.Check(continuationCtx, pressure.WorkLifecycle); err != nil {
		return nil, false
	}
	admission.continuationUsed = true
	return continuationCtx, true
}

func transitionRecoveryMustYield(ctx context.Context) bool {
	admission := transitionRecoveryAdmissionFrom(ctx)
	return admission != nil && admission.continuationUsed && pressure.IsTransitionContinuation(ctx)
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
	transitionCtx := withTransitionRecoveryAdmission(ctx)
	transitionBlocked := m.recoverPendingTransitionRecords(transitionCtx, state)
	imageUpdateBlocked := m.recoverPendingImageUpdates(transitionCtx, state, transitionBlocked)
	manifestUpdateBlocked := m.recoverPendingManifestUpdates(transitionCtx, state, transitionBlocked)
	if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkLifecycle); err != nil {
		return
	}
	ownershipReconcileReady := true
	if err := m.reconcileArtifactReferences(ctx, state); err != nil {
		// Artifact ownership stays fail closed, but a corrupt owner must not
		// prevent unrelated installed apps from reconciling their runtime.
		log.Printf("ERROR: reconcile artifact references: %v", err)
		ownershipReconcileReady = false
	}
	if ownershipReconcileReady {
		if err := m.reconcileCapabilityDefaultsAndEffects(ctx, state); err != nil {
			log.Printf("ERROR: reconcile capability defaults: %v", err)
			return
		}
	} else {
		log.Printf("WARN: reconcile capability ownership deferred until app/artifact ownership is readable")
	}
	m.beginObservationPass()

	// Clean up orphaned per-app users on first reconciliation. On-disk
	// publications, including incomplete or unreadable ones, remain user
	// owners until their dedicated recovery path proves process absence.
	appDirectoryIDs, directoryErr := state.listAppDirectoryIDs()
	if directoryErr != nil {
		log.Printf("ERROR: orphan app-user cleanup: list app directories: %v", directoryErr)
	} else {
		knownIDs := make(map[string]bool)
		for _, app := range state.ListApps() {
			if app != nil {
				knownIDs[app.InstanceID] = true
			}
		}
		for _, instanceID := range appDirectoryIDs {
			knownIDs[instanceID] = true
		}
		m.orphanCleanupOnce.Do(func() {
			container.CleanupOrphanAppUsers(knownIDs)
		})
	}

	for _, appInst := range state.ListApps() {
		if ctx.Err() != nil {
			return
		}
		if appInst == nil || strings.TrimSpace(appInst.InstanceID) == "" {
			continue
		}
		if imageUpdateBlocked[appInst.InstanceID] {
			continue
		}
		if manifestUpdateBlocked[appInst.InstanceID] {
			continue
		}
		if transitionBlocked[appInst.InstanceID] {
			continue
		}
		if m.automaticRecoverySuppressed(appInst.InstanceID) {
			continue
		}
		if err := m.rejectIfTransitionInProgress(state, appInst.InstanceID, TransitionFenceNormalReconcile); err != nil {
			if !errors.Is(err, ErrTransitionInProgress) {
				log.Printf("ERROR: reconcile app %s: %v", appInst.InstanceID, err)
			}
			continue
		}
		releaseOwner := pressure.BeginLifecycleOwner("app:" + appInst.InstanceID)
		err := m.reconcileApp(ctx, state, appInst)
		releaseOwner()
		if err != nil {
			log.Printf("ERROR: reconcile app %s: %v", appInst.InstanceID, err)
		}
	}
}

func (m *AppManager) reconcileApp(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance) error {
	previousStatus, previousMessage := m.getObservedStatusAndMessage(appInst.InstanceID)
	if _, err := m.reconcilePartialRollback(ctx, state, appInst); err != nil {
		m.setObservedStatus(appInst.InstanceID, StatusError)
		return fmt.Errorf("recover pending rollback before runtime: %w", err)
	}

	follower := m.LastObservedRole(cluster.ResourceForApp(appInst.InstanceID)) == cluster.RoleFollower
	desiredRunning := appInst.Enabled
	if follower {
		desiredRunning = false
	}

	def, err := state.GetAppDefinition(appInst.InstanceID)
	if err != nil {
		if !desiredRunning {
			if !appInst.Enabled && m.serviceManager != nil {
				m.serviceManager.DeactivateApp(appInst.InstanceID)
			}
			if quiesceErr := m.quiesceAppUserSession(ctx, appInst.InstanceID); quiesceErr == nil {
				if follower && m.serviceManager != nil {
					m.serviceManager.DeactivateApp(appInst.InstanceID)
				}
				m.updateStatusWithEvent(appInst.InstanceID, StatusStopped)
				m.interruptStartupProbation(appInst.InstanceID)
				if !appInst.Enabled {
					m.clearStartupRecovery(appInst)
				}
				return nil
			} else {
				return errors.Join(err, quiesceErr)
			}
		}
		return err
	}

	if !desiredRunning {
		layout, layoutErr := m.ensureAppVolumeLayout(ctx, appInst.InstanceID)
		if layoutErr != nil {
			if !appInst.Enabled && m.serviceManager != nil {
				m.serviceManager.DeactivateApp(appInst.InstanceID)
			}
			if quiesceErr := m.quiesceAppUserSession(ctx, appInst.InstanceID); quiesceErr != nil {
				return errors.Join(layoutErr, quiesceErr)
			}
			if follower && m.serviceManager != nil {
				m.serviceManager.DeactivateApp(appInst.InstanceID)
			}
			m.finalizeQuiescedContainerGroup(ctx, appInst, def, piccoloModeFromExtensions(def.Extensions))
			if !appInst.Enabled {
				m.clearStartupRecovery(appInst)
			}
			m.clearUnknownObservation(appInst.InstanceID)
			return nil
		}
		if !appInst.Enabled && m.serviceManager != nil {
			m.serviceManager.DeactivateApp(appInst.InstanceID)
		}
		if err := m.quiesceContainerGroup(ctx, state, appInst, def, layout); err != nil {
			return err
		}
		// Manual Stop may have committed Enabled=false before runtime cleanup
		// failed. A later disabled reconcile that completes the stop owns the
		// same successful stop boundary and clears the old startup budget. A
		// follower keeps Enabled=true and therefore retains that history.
		if !appInst.Enabled {
			m.clearStartupRecovery(appInst)
		}
		m.clearUnknownObservation(appInst.InstanceID)
		return nil
	}

	mode := piccoloModeFromExtensions(def.Extensions)
	layout, layoutObserveErr := m.observeAppVolumeLayout(ctx, appInst.InstanceID)
	if layoutObserveErr != nil && !errors.Is(layoutObserveErr, errAppVolumeObservationUnavailable) {
		log.Printf("WARN: reconcile app %s: volume observation unknown: %v", appInst.InstanceID, layoutObserveErr)
		m.recordUnknownObservation(appInst.InstanceID, layoutObserveErr)
		return nil
	}
	var runtime container.PodmanRuntime
	var runtimeObserveErr error
	if layoutObserveErr == nil {
		runtime, runtimeObserveErr = m.podmanRuntimeForApp(ctx, appInst.InstanceID, layout, mode, appRuntimeObserve)
	} else {
		runtimeObserveErr = layoutObserveErr
	}
	var observed containerGroupObservation
	if runtimeObserveErr == nil {
		observed = m.observeContainerGroup(ctx, runtime, appInst, def)
		if !observed.known() {
			log.Printf("WARN: reconcile app %s: container group observation unknown: %v", appInst.InstanceID, observed.Err)
			return m.handleUnknownContainerGroup(ctx, state, appInst, def, layout, mode, observed.Err)
		}
		m.clearUnknownObservation(appInst.InstanceID)
		if observed.Outcome == containerGroupRunning {
			err := m.reconcileContainerGroup(ctx, state, appInst, def, layout, runtime, true, observed)
			if err != nil {
				if pressure.IsAdmissionError(err) {
					m.pauseStartupAttemptForAdmission(state, appInst, previousStatus, previousMessage)
				} else if m.startupAttemptActive(appInst.InstanceID) {
					m.handleStartupFailure(state, appInst)
				}
				return err
			}
			m.markStartupRecoverySucceeded(state, appInst)
			return nil
		}
	} else if !errors.Is(runtimeObserveErr, container.ErrUserSessionUnavailable) &&
		!errors.Is(runtimeObserveErr, errAppRuntimeObservationUnavailable) &&
		!errors.Is(runtimeObserveErr, errAppVolumeObservationUnavailable) {
		log.Printf("WARN: reconcile app %s: runtime observation unknown: %v", appInst.InstanceID, runtimeObserveErr)
		return m.handleUnknownContainerGroup(ctx, state, appInst, def, layout, mode, runtimeObserveErr)
	}

	if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkLifecycle); err != nil {
		return nil
	}
	projectStarting := previousStatus != StatusRunning
	if !m.beginAutomaticStartupAttemptWithProjection(state, appInst, projectStarting) {
		return nil
	}
	if runtimeObserveErr != nil {
		if layoutObserveErr != nil {
			layout, err = m.ensureAppVolumeLayout(ctx, appInst.InstanceID)
			if err != nil {
				if pressure.IsAdmissionError(err) {
					m.pauseStartupAttemptForAdmission(state, appInst, previousStatus, previousMessage)
				} else if projectStarting {
					m.handleStartupFailure(state, appInst)
				} else {
					m.finishUnknownObservation(state, appInst)
				}
				return err
			}
		}
		runtime, err = m.podmanRuntimeForApp(ctx, appInst.InstanceID, layout, mode, appRuntimeEnsureReady)
		if err != nil {
			if pressure.IsAdmissionError(err) {
				m.pauseStartupAttemptForAdmission(state, appInst, previousStatus, previousMessage)
			} else if projectStarting {
				m.handleStartupFailure(state, appInst)
			} else {
				m.finishUnknownObservation(state, appInst)
			}
			return err
		}
		observed = m.observeContainerGroup(ctx, runtime, appInst, def)
		if !observed.known() {
			log.Printf("WARN: reconcile app %s: container group observation remains unknown after runtime repair: %v", appInst.InstanceID, observed.Err)
			return m.handleUnknownContainerGroup(ctx, state, appInst, def, layout, mode, observed.Err)
		}
		m.clearUnknownObservation(appInst.InstanceID)
	}

	// Unified reconcile path: all apps (service and workspace) use one complete
	// observation snapshot. Unknown never reaches this effect boundary.
	if err := m.reconcileContainerGroup(ctx, state, appInst, def, layout, runtime, true, observed); err != nil {
		if pressure.IsAdmissionError(err) {
			m.pauseStartupAttemptForAdmission(state, appInst, previousStatus, previousMessage)
		} else if m.startupAttemptActive(appInst.InstanceID) {
			m.handleStartupFailure(state, appInst)
		}
		return err
	}
	m.markStartupRecoverySucceeded(state, appInst)
	return nil
}

func (m *AppManager) ensureServicesForRunningApp(ctx context.Context, def *api.AppDefinition, instanceID, containerID string, runtime container.PodmanRuntime) error {
	if m.serviceManager == nil {
		return nil
	}
	if _, err := m.serviceManager.GetByApp(instanceID); err == nil &&
		m.serviceManager.AppPublicationActive(instanceID) {
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

	if _, err := m.serviceManager.RestoreFromPodmanContext(ctx, instanceID, def.Listeners, ports); err != nil {
		if errors.Is(err, services.ErrPublicationRestoreIncomplete) {
			return fmt.Errorf("%w: %v", container.ErrPortReconciliationRequired, err)
		}
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
	defer pressure.BeginLifecycleOwner("app:install")()
	if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkLifecycle); err != nil {
		return nil, err
	}
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
	var capabilityPending *CapabilitySelectionReconcilePendingError
	if err == nil || (inst != nil && errors.As(err, &capabilityPending)) {
		m.ReconcileAllSlicePolicies()
	}
	return inst, err
}

func (m *AppManager) installLocked(ctx context.Context, appDef *api.AppDefinition) (inst *AppInstance, err error) {
	instanceID := ""
	m.emitProgress(ctx, taskTypeInstallApp, instanceID, taskPhaseValidating, 0, "Validating app manifest", false, nil)
	defer func() {
		if err != nil {
			var pending *CapabilitySelectionReconcilePendingError
			if errors.As(err, &pending) && inst != nil {
				instanceID = inst.InstanceID
				m.emitProgress(ctx, taskTypeInstallApp, instanceID, taskPhaseComplete, 100, "Install complete; capability reconciliation pending", true, nil)
				return
			}
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
		taskType, progress := m.inheritedTaskProgress(ctx, taskTypeInstallApp, 10)
		m.emitProgress(
			ctx,
			taskType,
			instanceID,
			taskPhaseAllocatingPorts,
			progress,
			fmt.Sprintf("Retrying installation (attempt %d)", attempt+1),
			false,
			nil,
		)
	}

	layout, err := m.ensureAppVolumeLayout(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	runtime, err := m.podmanRuntimeForApp(ctx, instanceID, layout, piccoloModeFromExtensions(appDef.Extensions), appRuntimeEnsureReady)
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
	app, err := m.installContainerGroup(ctx, appDef, instanceID, layout, runtime, endpoints, nil, false, true)
	if err != nil {
		if uncommittedContainerGroupMaySurvive(err) {
			// The candidate still owns the app volume, rootfs, artifact
			// attachments, and possibly live processes. Preserve those resources
			// for an explicit retry or restart to reconcile safely.
			cleanupResources = false
			return nil, err
		}
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
		storeErr := fmt.Errorf("failed to store app: %w", err)
		if cleanupErr := m.compensateUncommittedContainerGroup(
			state,
			nil,
			app,
			appDef,
			runtime,
		); cleanupErr != nil {
			// A surviving process still owns the app volume and user. Preserve
			// those resources so reconciliation/administrative cleanup can prove
			// absence before destroying them.
			cleanupResources = false
			return nil, errors.Join(
				storeErr,
				fmt.Errorf("compensate uncommitted container group: %w", cleanupErr),
			)
		}
		if cleanupErr := state.removeIncompleteApp(instanceID); cleanupErr != nil {
			// Process absence is proven, but keep the deterministic user,
			// volume, and artifact identities intact so background reconcile
			// can repeat the proof before retrying publication cleanup.
			cleanupResources = false
			return nil, errors.Join(
				storeErr,
				fmt.Errorf("remove incomplete app publication: %w", cleanupErr),
			)
		}
		return nil, storeErr
	}

	// StoreApp is the ordinary install commit. From this point the persisted app
	// owns its resources even if first-provider capability effects need repair.
	// In particular, a reconciliation-pending return must not run candidate
	// cleanup against the now-committed runtime.
	cleanupResources = false
	cleanupServices = false

	// Atomically set observed status and clear any transient message, then populate for API callers.
	m.observedStatusMu.Lock()
	m.observedStatus[instanceID] = StatusRunning
	m.observedStatusMessage[instanceID] = ""
	m.observedStatusMu.Unlock()
	app.Status = StatusRunning

	// The first installed provider becomes the automatic default only after its
	// ordinary install has committed. Converge that default and its runtime
	// effects before returning while this lifecycle still owns reconcileMu.
	// Other provider installs do not steal or re-finalize an existing default.
	providedCapabilities := make([]string, 0, len(registeredCapabilities()))
	for _, capability := range registeredCapabilities() {
		if _, _, provides := providedCapability(appDef, capability); provides {
			providedCapabilities = append(providedCapabilities, capability)
		}
	}
	var capabilityReconcileErr error
	if len(providedCapabilities) > 0 {
		durable, loadErr := state.loadCapabilityState()
		if loadErr != nil {
			capabilityReconcileErr = loadErr
		} else {
			for _, capability := range providedCapabilities {
				if durable.Defaults[capability] == "" {
					capabilityReconcileErr = m.finalizeCommittedCapabilityRuntime(ctx, state, instanceID)
					break
				}
			}
		}
	}

	app.Status, app.StatusMessage = m.getObservedStatusAndMessage(instanceID)
	if app.Status == "" {
		app.Status = StatusRunning
	}
	m.publishAppStatusChanged(instanceID, "installed", "", "")
	if capabilityReconcileErr != nil {
		return app, &CapabilitySelectionReconcilePendingError{Cause: capabilityReconcileErr}
	}
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

func (m *AppManager) destroyAppUser(instanceID string) error {
	if m.appUserDestroyer != nil {
		return m.appUserDestroyer(instanceID)
	}
	return container.DestroyAppUser(instanceID)
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
func (m *AppManager) CloneWorkspace(ctx context.Context, originID, cloneID string) (inst *AppInstance, err error) {
	m.emitProgress(ctx, taskTypeCloneApp, cloneID, taskPhaseValidating, 0, "Validating workspace clone", false, nil)
	defer func() {
		if err != nil {
			m.emitProgress(ctx, taskTypeCloneApp, cloneID, taskPhaseComplete, 100, "Clone failed", true, err)
			return
		}
		m.emitProgress(ctx, taskTypeCloneApp, cloneID, taskPhaseComplete, 100, "Clone complete", true, nil)
	}()
	if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkLifecycle); err != nil {
		return nil, err
	}
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
	originInst, exists := state.GetApp(originID)
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
	runtime, err := m.podmanRuntimeForApp(ctx, cloneID, layout, ModeWorkspace, appRuntimeEnsureReady)
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
	cloneInst, err := m.installContainerGroup(ctx, &cloneDef, cloneID, layout, runtime, endpoints, prebuiltRootfs, false, false)
	if err != nil {
		if uncommittedContainerGroupMaySurvive(err) {
			cleanupResources = false
			cleanupRootfs = false
		}
		return nil, fmt.Errorf("clone %s from %s: install containers: %w", cloneID, originID, err)
	}

	// Set clone provenance.
	cloneInst.ClonedFrom = originID
	cloneInst.Init = cloneInitState(originInst.Init)

	// Persist clone state.
	if err := state.StoreApp(cloneInst); err != nil {
		storeErr := fmt.Errorf("clone %s from %s: persist state: %w", cloneID, originID, err)
		cleanupErr := m.compensateUncommittedContainerGroup(
			state,
			nil,
			cloneInst,
			&cloneDef,
			runtime,
		)
		if cleanupErr != nil {
			cleanupResources = false
			cleanupRootfs = false
			return nil, errors.Join(
				storeErr,
				fmt.Errorf("compensate uncommitted container group: %w", cleanupErr),
			)
		}
		if cleanupErr := state.removeIncompleteApp(cloneID); cleanupErr != nil {
			cleanupResources = false
			cleanupRootfs = false
			return nil, errors.Join(
				storeErr,
				fmt.Errorf("remove incomplete clone publication: %w", cleanupErr),
			)
		}
		return nil, storeErr
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
		m.decorateAppResponseState(state, &copy)
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
	m.decorateAppResponseState(state, &app)
	return &app, nil
}

func (m *AppManager) decorateAppResponseState(state *FilesystemStateManager, app *AppInstance) {
	if state == nil || app == nil || strings.TrimSpace(app.InstanceID) == "" {
		return
	}
	record, err := state.LoadTransitionRecord(app.InstanceID)
	if err == nil && record != nil && record.Phase != TransitionPhaseCommitted {
		app.TransitionActive = true
		app.TransitionOperation = string(record.Plan.OperationKind)
		app.TransitionPhase = string(record.Phase)
		app.TransitionMessage = transitionDisplayMessage(record)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("WARN: app response %s: load transition record: %v", app.InstanceID, err)
	}

	txn, err := state.LoadManifestUpdateTransaction(app.InstanceID)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		log.Printf("WARN: app response %s: load manifest update transaction: %v", app.InstanceID, err)
		return
	}
	if txn == nil || txn.AccessPublished || txn.Phase != "publishing_access" {
		return
	}
	app.AccessRepairPending = true
	message := strings.TrimSpace(txn.LastError)
	if message == "" {
		message = "Update committed, but access publication needs repair."
	} else if !strings.HasPrefix(message, "Update committed") {
		message = "Update committed, but access publication needs repair: " + message
	}
	app.AccessRepairMessage = message
}

func transitionDisplayMessage(record *TransitionRecord) string {
	if record == nil {
		return "An app update is still finishing."
	}
	operation := transitionOperationDisplayLabel(record.Plan.OperationKind)
	phase := transitionPhaseDisplayLabel(record.Phase)
	if strings.TrimSpace(record.LastError) != "" && record.Phase == TransitionPhaseRestoreFailed {
		return operation + " needs repair: " + strings.TrimSpace(record.LastError)
	}
	return operation + " is " + phase + "."
}

func transitionOperationDisplayLabel(operation TransitionOperationKind) string {
	switch operation {
	case TransitionOperationUpdateImage:
		return "Image refresh"
	case TransitionOperationModifyApp, TransitionOperationCatalogManifestReview:
		return "App update"
	case TransitionOperationEditConfig, TransitionOperationCatalogConfigReview:
		return "Config update"
	case TransitionOperationCatalogAutoApply:
		return "Catalog update"
	case TransitionOperationAccessRepair:
		return "Access repair"
	case TransitionOperationCleanupRetry:
		return "Cleanup"
	case TransitionOperationMetadataRetry:
		return "Metadata update"
	default:
		return "App update"
	}
}

func transitionPhaseDisplayLabel(phase TransitionPhase) string {
	switch phase {
	case TransitionPhasePrepared, TransitionPhaseResourcesPrepared:
		return "preparing"
	case TransitionPhaseCommitIntent, TransitionPhaseSourceCommitting, TransitionPhaseSourceCommitted:
		return "committing"
	case TransitionPhaseSwitchingRuntime, TransitionPhaseCandidateTouched:
		return "switching runtime"
	case TransitionPhasePublishingAccess:
		return "publishing access"
	case TransitionPhaseCommittedMetadataPending:
		return "saving metadata"
	case TransitionPhaseCommittedCleanupPending:
		return "finishing cleanup"
	case TransitionPhaseRestoringPrevious:
		return "restoring the previous app"
	case TransitionPhaseRestoreFailed:
		return "waiting for repair"
	case TransitionPhaseCommitted:
		return "complete"
	default:
		return "finishing"
	}
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
	defer pressure.BeginLifecycleOwner("app:" + instanceID)()
	if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkLifecycle); err != nil {
		return err
	}
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()
	state, err := m.ensureStateManager()
	if err != nil {
		return err
	}
	if err := m.ensureUnlocked(); err != nil {
		return err
	}
	if err := m.ensureKernelLeader(); err != nil {
		return err
	}
	if err := m.recoverPendingImageUpdateBeforeTransitionFence(ctx, state, instanceID, "start"); err != nil {
		return err
	}
	if err := m.rejectIfTransitionInProgress(state, instanceID, TransitionFenceStart); err != nil {
		return err
	}
	if _, exists := state.GetApp(instanceID); !exists {
		return fmt.Errorf("app instance not found: %s", instanceID)
	}
	// A validated manual Start is the explicit retry authority for an app whose
	// automatic recovery was circuit-broken. Storage/leadership/transition or
	// not-found failures above must not silently re-enable the culprit.
	m.clearAutomaticRecoverySuppression(instanceID)
	return m.startLocked(ctx, instanceID)
}

func (m *AppManager) recoverPendingImageUpdateBeforeTransitionFence(ctx context.Context, state *FilesystemStateManager, instanceID, operation string) error {
	record, err := state.LoadTransitionRecord(instanceID)
	if err == nil && record != nil && record.Phase != TransitionPhaseCommitted {
		if record.Plan.OperationKind != TransitionOperationUpdateImage || record.Plan.SourceKind != TransitionSourceCurrentCommitted {
			return nil
		}
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read app transition record before %s: %w", operation, err)
	}
	appInst, exists := state.GetApp(instanceID)
	if !exists {
		return nil
	}
	if _, recoverErr := m.recoverPendingImageUpdateForApp(ctx, state, appInst); recoverErr != nil {
		m.setObservedStatus(instanceID, StatusError)
		return fmt.Errorf("recover pending image update before %s: %w", operation, recoverErr)
	}
	return nil
}

func (m *AppManager) startLocked(ctx context.Context, instanceID string) (err error) {
	previousStatus, previousMessage := m.getObservedStatusAndMessage(instanceID)
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
	if recovered, recoverErr := m.recoverPendingImageUpdateForApp(ctx, state, app); recoverErr != nil {
		m.setObservedStatus(instanceID, StatusError)
		return fmt.Errorf("recover pending image update before start: %w", recoverErr)
	} else if recovered {
		var ok bool
		app, ok = state.GetApp(instanceID)
		if !ok {
			return fmt.Errorf("app instance not found after image update recovery: %s", instanceID)
		}
	}
	if !app.Enabled {
		if err := state.UpdateAppEnabled(instanceID, true); err != nil {
			return fmt.Errorf("failed to persist enabled state: %w", err)
		}
		var ok bool
		app, ok = state.GetApp(instanceID)
		if !ok {
			return fmt.Errorf("app instance not found after enabling: %s", instanceID)
		}
	}
	// A manual start is a new recovery observation/effect. It must not reuse
	// healthy time accumulated before the loss that prompted this request.
	m.interruptStartupProbation(instanceID)
	defer func() {
		if err != nil {
			if pressure.IsAdmissionError(err) {
				m.pauseStartupAttemptForAdmission(state, app, previousStatus, previousMessage)
			} else if !m.startupAttemptActive(instanceID) {
				m.handleStartupFailure(state, app)
			}
			return
		}
		m.markStartupRecoverySucceeded(state, app)
	}()

	layout, err := m.ensureAppVolumeLayout(ctx, instanceID)
	if err != nil {
		return err
	}

	def, defErr := state.GetAppDefinition(instanceID)
	if defErr != nil {
		return fmt.Errorf("failed to load app definition: %w", defErr)
	}

	runtime, err := m.podmanRuntimeForApp(ctx, instanceID, layout, piccoloModeFromExtensions(def.Extensions), appRuntimeEnsureReady)
	if err != nil {
		return err
	}
	// Unified start path for all app modes (container group: network anchor + services)
	m.emitProgress(ctx, taskTypeStartApp, instanceID, taskPhaseStarting, 60, "Starting containers", false, nil)
	if strings.TrimSpace(app.NetworkAnchorID) == "" {
		bn, rootfsErr := m.ensureAllServiceRootfsAttached(ctx, instanceID, piccoloModeFromExtensions(def.Extensions), def, app)
		if rootfsErr != nil {
			return fmt.Errorf("failed to attach rootfs: %w", rootfsErr)
		}
		if m.serviceManager != nil {
			m.serviceManager.DeactivateApp(instanceID)
		}
		if stopErr := m.stopContainersForMultiApp(ctx, app, def, runtime); stopErr != nil {
			log.Printf("WARN: start %s: pre-recreate stop failed: %v", instanceID, stopErr)
		}
		if removeErr := m.removeContainersForMultiApp(ctx, app, def, runtime); removeErr != nil {
			log.Printf("WARN: start %s: pre-recreate remove failed: %v", instanceID, removeErr)
		}
		if m.serviceManager != nil {
			m.serviceManager.DeactivateApp(instanceID)
		}
		return m.recreateMissingMultiContainer(ctx, state, app, def, layout, runtime, bn)
	}
	return m.startContainerGroup(ctx, state, app, def, layout, runtime)
}

// Stop stops an application instance by instanceID.
func (m *AppManager) Stop(ctx context.Context, instanceID string) error {
	defer pressure.BeginLifecycleOwner("app:" + instanceID)()
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
	if err := m.rejectIfTransitionInProgress(state, instanceID, TransitionFenceStop); err != nil {
		return err
	}
	return m.stopInternal(ctx, instanceID)
}

func (m *AppManager) stopForFollowerTransition(ctx context.Context, instanceID string) error {
	// The observed follower role is the authority for this quiescence. It must
	// be able to stop the local app through Warning, while Critical continues
	// to take precedence through the admission gate's hard fence.
	ctx = pressure.WithTransitionContinuation(ctx)

	state, err := m.ensureStateManager()
	if err != nil {
		return err
	}
	app, exists := state.GetApp(instanceID)
	if !exists {
		return nil
	}

	finish := func() {
		if m.serviceManager != nil {
			m.serviceManager.DeactivateApp(instanceID)
		}
		m.updateStatusWithEvent(instanceID, StatusStopped)
		m.interruptStartupProbation(instanceID)
		m.clearUnknownObservation(instanceID)
	}

	// Follower demotion is a transition-boundary quiesce exception: it may stop
	// local containers, but must not change app source/rootfs metadata or consume
	// transition records. The leader/recovery path owns the active transition.
	def, defErr := state.GetAppDefinition(instanceID)
	if defErr != nil {
		if err := m.quiesceAppUserSession(ctx, instanceID); err != nil {
			return errors.Join(fmt.Errorf("read app definition before follower quiesce: %w", defErr), err)
		}
		finish()
		return nil
	}

	layout, err := m.ensureAppVolumeLayout(ctx, instanceID)
	if err != nil {
		if quiesceErr := m.quiesceAppUserSession(ctx, instanceID); quiesceErr != nil {
			return errors.Join(fmt.Errorf("resolve volume layout before follower quiesce: %w", err), quiesceErr)
		}
		finish()
		return nil
	}
	mode := piccoloModeFromExtensions(def.Extensions)
	runtime, sessionQuiesced, err := m.quiesceRuntimeForApp(ctx, instanceID, layout, mode)
	if err != nil {
		return err
	}
	if !sessionQuiesced {
		if stopErr := m.stopContainersForMultiApp(ctx, app, def, runtime); stopErr != nil {
			// A grouped graceful stop is not atomic: one container may already
			// be down when another stop reports an error. From this point the
			// old publication can no longer truthfully represent a complete
			// backend, even if PID 1 cannot subsequently prove full quiescence.
			if m.serviceManager != nil {
				m.serviceManager.DeactivateApp(instanceID)
			}
			log.Printf("WARN: follower demotion graceful stop failed for %s, quiescing dedicated user unit: %v", instanceID, stopErr)
			if quiesceErr := m.quiesceAppUserSession(ctx, instanceID); quiesceErr != nil {
				return errors.Join(stopErr, quiesceErr)
			}
		}
	}
	finish()
	return nil
}

func (m *AppManager) stopInternal(ctx context.Context, instanceID string) (err error) {
	m.emitProgress(ctx, taskTypeStopApp, instanceID, taskPhaseStopping, 0, "Stopping app", false, nil)
	defer func() {
		if err == nil {
			m.clearUnknownObservation(instanceID)
		}
	}()
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
	if err := state.UpdateAppEnabled(instanceID, false); err != nil {
		return fmt.Errorf("failed to persist disabled state: %w", err)
	}
	// Enabled=false is now durable, so this manual lifecycle owner must be able
	// to reach the quiescence boundary even if task pressure entered Warning
	// between the request and the cleanup commands.
	ctx = pressure.WithTransitionContinuation(ctx)
	if m.serviceManager != nil {
		m.serviceManager.DeactivateApp(instanceID)
	}

	def, defErr := state.GetAppDefinition(instanceID)
	if defErr != nil {
		if m.serviceManager != nil {
			m.serviceManager.DeactivateApp(instanceID)
		}
		if quiesceErr := m.quiesceAppUserSession(ctx, instanceID); quiesceErr != nil {
			return errors.Join(fmt.Errorf("failed to load app definition: %w", defErr), quiesceErr)
		}
		if err := m.detachArtifactReferences(ctx, app.ArtifactReferences); err != nil {
			return fmt.Errorf("detach artifact references after fallback quiescence: %w", err)
		}
		m.updateStatusWithEvent(instanceID, StatusStopped)
		if m.serviceManager != nil {
			m.serviceManager.DeactivateApp(instanceID)
		}
		m.interruptStartupProbation(instanceID)
		m.clearStartupRecovery(app)
		return nil
	}

	layout, err := m.ensureAppVolumeLayout(ctx, instanceID)
	if err != nil {
		if m.serviceManager != nil {
			m.serviceManager.DeactivateApp(instanceID)
		}
		if quiesceErr := m.quiesceAppUserSession(ctx, instanceID); quiesceErr != nil {
			return errors.Join(fmt.Errorf("resolve volume layout before stop: %w", err), quiesceErr)
		}
		m.finalizeQuiescedContainerGroup(ctx, app, def, piccoloModeFromExtensions(def.Extensions))
		m.clearStartupRecovery(app)
		return nil
	}

	// Unified stop path for all app modes (container group: network anchor + services)
	m.emitProgress(ctx, taskTypeStopApp, instanceID, taskPhaseStopping, 40, "Stopping containers", false, nil)
	if err := m.quiesceContainerGroup(ctx, state, app, def, layout); err != nil {
		return err
	}
	m.clearStartupRecovery(app)
	return nil
}

// Uninstall removes an application instance completely by instanceID,
// including all container data, encrypted volumes, and podman state.
func (m *AppManager) Uninstall(ctx context.Context, instanceID string) error {
	return m.uninstall(ctx, instanceID, true)
}

func (m *AppManager) UninstallAcknowledged(ctx context.Context, instanceID string, acknowledged bool) error {
	return m.uninstall(ctx, instanceID, acknowledged)
}

func (m *AppManager) uninstall(ctx context.Context, instanceID string, acknowledged bool) error {
	defer pressure.BeginLifecycleOwner("app:" + instanceID)()
	if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkLifecycle); err != nil {
		return err
	}
	m.reconcileMu.Lock()
	state, stateErr := m.ensureStateManager()
	if stateErr != nil {
		m.reconcileMu.Unlock()
		return stateErr
	}
	if err := m.rejectIfTransitionInProgress(state, instanceID, TransitionFenceUninstall); err != nil {
		m.reconcileMu.Unlock()
		return err
	}
	capability, selected, err := capabilitySelectedByProvider(state, instanceID)
	if err != nil {
		m.reconcileMu.Unlock()
		return err
	}
	if selected && !acknowledged {
		m.reconcileMu.Unlock()
		return &CapabilityProviderChangeConfirmationRequiredError{
			Capability: capability,
			Current:    instanceID,
		}
	}

	// Remove the slice drop-in before the user is destroyed. Live-reset is
	// unnecessary because the slice itself is about to be torn down.
	m.RemoveSlicePolicyForApp(instanceID)

	err = m.uninstallLocked(ctx, instanceID)
	uninstalled := err == nil
	if err == nil && selected {
		if reconcileErr := m.reconcileCapabilityDefaultsAndEffects(ctx, state, instanceID); reconcileErr != nil {
			err = &CapabilitySelectionReconcilePendingError{Cause: reconcileErr}
		}
	}
	m.reconcileMu.Unlock()

	// Recompute slice policies for remaining apps: num_active_elastic may
	// have changed. Runs outside reconcileMu (systemctl calls).
	if uninstalled {
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
	// Persist the existing desired-state bit before teardown. If any later
	// cleanup proof fails, the retained app record is both the retry owner and
	// a durable instruction not to restart partially removed resources.
	if err := state.UpdateAppEnabled(instanceID, false); err != nil {
		return fmt.Errorf("disable app before uninstall: %w", err)
	}

	// Mark as uninstalling early so concurrent readers (log streams, exec) get a clean rejection
	// instead of racing against infrastructure teardown. Rollback on failure so the app doesn't
	// get stuck in "uninstalling" permanently.
	prevStatus := m.getObservedStatus(instanceID)
	quiesced := false
	m.updateStatusWithEvent(instanceID, StatusUninstalling)
	defer func() {
		if err != nil {
			rollback := prevStatus
			if quiesced {
				rollback = StatusStopped
			}
			if rollback == "" {
				rollback = StatusStopped
			}
			m.updateStatusWithEvent(instanceID, rollback)
		}
	}()
	if m.serviceManager != nil {
		m.serviceManager.DeactivateApp(instanceID)
	}

	layout, err := m.ensureAppVolumeLayout(ctx, instanceID)
	if err != nil {
		return err
	}

	def, defErr := state.GetAppDefinition(instanceID)
	if defErr != nil {
		return fmt.Errorf("failed to load app definition: %w", defErr)
	}

	mode := piccoloModeFromExtensions(def.Extensions)
	runtime, runtimeUsable, err := m.quiesceContainerGroupRuntime(ctx, state, app, def, layout)
	if err != nil {
		return err
	}
	quiesced = true
	if err := m.revokeAcceleratorAccess(ctx, state, instanceID); err != nil {
		return fmt.Errorf("revoke accelerator access before uninstall: %w", err)
	}

	m.emitProgress(ctx, taskTypeUninstallApp, instanceID, taskPhaseRemovingContainer, 40, "Removing containers", false, nil)
	if !runtimeUsable {
		if m.serviceManager != nil {
			m.serviceManager.RemoveApp(instanceID)
		}
	} else {
		if err := m.uninstallContainerGroup(ctx, app, def, runtime); err != nil {
			return err
		}
	}
	m.removeCapabilityIngresses(instanceID)
	if err := m.pruneCapabilityIngresses(state, instanceID, nil); err != nil {
		return fmt.Errorf("remove capability ingress state: %w", err)
	}

	// Keep artifact references until the app record is removed. If any later
	// uninstall step fails, the disabled app still owns exact reconstructible
	// identities and a retry never observes an app pointing at deleted refs.
	m.destroyAllServiceRootfs(ctx, instanceID, mode, def)

	// Reset podman storage BEFORE unmounting the volume.
	// This allows podman to properly clean its metadata files (db.sql, locks, etc.)
	// which live inside the encrypted volume.
	m.emitProgress(ctx, taskTypeUninstallApp, instanceID, taskPhaseCleaningVolumes, 80, "Purging app data", false, nil)
	if runtimeUsable {
		if err := m.containerManager.ResetStorage(ctx, runtime); err != nil {
			log.Printf("WARN: podman storage reset for %s failed: %v", instanceID, err)
		}
		// ResetStorage is the final rootless command in this teardown. Quiesce
		// once more afterward so volume destruction is gated not only on the
		// container scopes, but also on UID-owned Podman helpers that may have
		// been launched outside the dedicated user cgroup.
		if err := m.quiesceAppUserSession(ctx, instanceID); err != nil {
			return fmt.Errorf("quiesce per-app processes before purging data: %w", err)
		}
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
	runRoot := runtime.RunRoot
	if runRoot == "" {
		runRoot = filepath.Join(podmanRunRootBase(), volID)
	}
	if err := os.RemoveAll(runRoot); err != nil {
		log.Printf("WARN: failed to remove podman runRoot %s: %v", runRoot, err)
	}
	serviceRoot := runtime.Root
	if serviceRoot == "" {
		serviceRoot = paths.PodmanJoin("apps", instanceID)
	}
	if err := os.RemoveAll(serviceRoot); err != nil {
		log.Printf("WARN: failed to remove podman service root %s: %v", serviceRoot, err)
	}

	// User/session cleanup is part of successful uninstall. Keep the disabled
	// app record until it succeeds so a later uninstall can retry safely.
	if err := m.destroyAppUser(instanceID); err != nil {
		return fmt.Errorf("failed to destroy per-app user: %w", err)
	}

	// Remove from filesystem and cache (state only)
	if err := state.RemoveApp(instanceID); err != nil {
		return fmt.Errorf("failed to remove app from storage: %w", err)
	}
	if err := m.destroyArtifactReferences(ctx, app.ArtifactReferences); err != nil {
		// The app is already authoritatively absent. Startup reconciliation
		// derives retained references from installed app records and will reap
		// this bounded cleanup debt.
		log.Printf("WARN: uninstall %s: destroy orphaned artifact references: %v", instanceID, err)
	} else if rootfs := m.currentRootfsManager(); rootfs != nil {
		if err := rootfs.GarbageCollectGoldenLVs(ctx); err != nil {
			log.Printf("WARN: uninstall %s: garbage collect golden content: %v", instanceID, err)
		}
	}
	m.clearStartupRecovery(app)
	m.retireRuntimeObservation(instanceID)

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
	defer pressure.BeginLifecycleOwner("app:" + instanceID)()
	if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkLifecycle); err != nil {
		return err
	}
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()
	state, err := m.ensureStateManager()
	if err != nil {
		return err
	}
	if err := m.rejectIfTransitionInProgress(state, instanceID, TransitionFenceUpdateImage); err != nil {
		return err
	}
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

	runtime, err := m.podmanRuntimeForApp(ctx, instanceID, layout, mode, appRuntimeEnsureReady)
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
	if err := rejectImageUpdateIfLegacyJournalPending(state, instanceID); err != nil {
		return err
	}
	if err := m.preflightImageRefreshRollbackSnapshot(ctx, instanceID, curDef); err != nil {
		return err
	}
	imagePlan, err := m.resolveUpdateImageRootfsPlan(ctx, instanceID, appInst, updatedImages)
	if err != nil {
		return fmt.Errorf("resolve image update plan: %w", err)
	}
	if len(imagePlan) == 0 {
		log.Printf("INFO: update image %s: registry digests match active rootfs; nothing to refresh", instanceID)
		return nil
	}
	transitionRecord, err := m.beginUpdateImageTransitionRecord(ctx, state, appInst, curDef, imagePlan)
	if err != nil {
		return err
	}
	if transitionRecord != nil {
		defer func() {
			if err == nil {
				return
			}
			if _, loadErr := state.LoadImageUpdateTransaction(instanceID); errors.Is(loadErr, os.ErrNotExist) {
				record, recordErr := state.LoadTransitionRecord(instanceID)
				if errors.Is(recordErr, os.ErrNotExist) {
					return
				}
				if recordErr != nil {
					log.Printf("WARN: update image %s: inspect transition record after failure: %v", instanceID, recordErr)
					return
				}
				if record.Phase == TransitionPhasePrepared {
					if clearErr := state.ClearTransitionRecord(instanceID); clearErr != nil {
						log.Printf("WARN: update image %s: clear pre-resource transition record: %v", instanceID, clearErr)
					}
					return
				}
				record.LastError = err.Error()
				if storeErr := state.StoreTransitionRecord(instanceID, record); storeErr != nil {
					log.Printf("WARN: update image %s: persist transition failure details: %v", instanceID, storeErr)
				}
			} else if loadErr != nil {
				log.Printf("WARN: update image %s: inspect rollback journal after failure: %v", instanceID, loadErr)
			}
		}()
	}

	// RollbackToSnapshot reads app.prev.yaml to restore pre-update state — keep
	// it in sync even though Update itself doesn't mutate the manifest.
	if err := state.BackupCurrentAppDefinition(instanceID); err != nil {
		return fmt.Errorf("backup app.yaml: %w", err)
	}

	// Service-mode: transactional rootfs update (RFC 20260302).
	if err := m.updateServiceModeImage(ctx, state, appInst, curDef, layout, runtime, imagePlan); err != nil {
		return err
	}
	if transitionRecord != nil {
		if imageTxn, loadErr := state.LoadImageUpdateTransaction(instanceID); loadErr == nil && imageTxn != nil {
			if storeErr := storeTransitionRecordForImageUpdate(state, instanceID, imageTxn, appInst); storeErr != nil {
				log.Printf("WARN: update image %s: retain transition for uncleared image journal: %v", instanceID, storeErr)
			}
		} else if !errors.Is(loadErr, os.ErrNotExist) {
			log.Printf("WARN: update image %s: inspect committed image journal: %v", instanceID, loadErr)
		} else if storeErr := storeTransitionRecordForImageUpdate(state, instanceID, &ImageUpdateTransaction{Phase: imageUpdatePhaseCommitted}, appInst); storeErr != nil {
			log.Printf("WARN: update image %s: mark transition committed: %v", instanceID, storeErr)
		} else if clearErr := state.ClearTransitionRecord(instanceID); clearErr != nil {
			log.Printf("WARN: update image %s: clear committed transition record: %v", instanceID, clearErr)
		}
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

func (m *AppManager) beginUpdateImageTransitionRecord(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance, curDef *api.AppDefinition, imagePlan []ManifestUpdateImagePlanItem) (*TransitionRecord, error) {
	_ = ctx
	instanceID := appInst.InstanceID
	if err := rejectImageUpdateIfLegacyJournalPending(state, instanceID); err != nil {
		return nil, err
	}
	baseHash, err := canonicalManifestHash(curDef)
	if err != nil {
		return nil, fmt.Errorf("hash current manifest: %w", err)
	}
	runtimeFingerprint, err := manifestRuntimeFingerprint(appInst)
	if err != nil {
		return nil, fmt.Errorf("fingerprint runtime: %w", err)
	}
	_, ledgerRevision, ledgerSourceHash, err := loadInstallLedgerFingerprint(state, instanceID)
	if err != nil {
		return nil, err
	}
	imageRootfs := transitionImageRootfsFromManifestPlan(imagePlan)
	primary := primaryServiceFor(curDef, appInst)
	order, err := serviceStartOrder(curDef.Services)
	if err != nil {
		return nil, fmt.Errorf("service start order: %w", err)
	}
	candidateActiveRootfs := cloneStringMap(appInst.ActiveRootfs)
	if candidateActiveRootfs == nil {
		candidateActiveRootfs = map[string]string{}
	}
	resourceKeys := make(map[string]string, len(imagePlan)+len(order)+2)
	const candidateRuntimeNamePolicy = "deterministic_app_service_container_names_v1"
	resourceKeys["runtime:name_policy"] = candidateRuntimeNamePolicy
	resourceKeys["runtime:anchor"] = networkAnchorContainerName(instanceID)
	for _, svcName := range order {
		resourceKeys["runtime:service:"+svcName] = containerNameForService(instanceID, svcName, primary)
	}
	for _, item := range imagePlan {
		if strings.TrimSpace(item.ServiceName) != "" {
			resourceKeys["image:"+item.ServiceName] = item.RootfsVolumeID
			if strings.TrimSpace(item.RootfsVolumeID) != "" {
				candidateActiveRootfs[item.ServiceName] = item.RootfsVolumeID
			}
		}
	}
	snapshotRequired := appHasPersistentStorage(curDef)
	plan, err := PlanInstalledAppTransition(TransitionPlanInput{
		OperationKind:         TransitionOperationUpdateImage,
		SourceKind:            TransitionSourceCurrentCommitted,
		Mode:                  piccoloModeFromExtensions(curDef.Extensions),
		Enabled:               appInst.Enabled,
		RuntimeChanging:       true,
		BaseManifestHash:      baseHash,
		CandidateManifestHash: baseHash,
		LedgerRevision:        ledgerRevision,
		SourceHash:            ledgerSourceHash,
		ImageRootfs:           imageRootfs,
		Data: TransitionDataPolicy{
			SnapshotRequired:      snapshotRequired,
			CandidateMayTouchData: snapshotRequired,
			RollbackBehavior:      "restore_previous_data_before_commit_or_forward_complete_after_commit_intent",
		},
		Runtime: TransitionRuntimePolicy{
			RecreatePolicy:             "recreate_current_manifest_with_refreshed_rootfs",
			RuntimeFingerprint:         runtimeFingerprint,
			PreviousActiveRootfs:       cloneStringMap(appInst.ActiveRootfs),
			CandidateActiveRootfs:      candidateActiveRootfs,
			PrimaryService:             primary,
			CandidateRuntimeNamePolicy: candidateRuntimeNamePolicy,
			ReadinessPolicy:            "existing_image_update_runtime_checks",
		},
		Access: TransitionAccessPolicy{
			PrepareRequired:     false,
			PublicationStrategy: "preserve_existing_listener_topology",
		},
		Cleanup: TransitionCleanupPolicy{
			StagedRootfsKeys: transitionRootfsKeysFromManifestPlan(imagePlan),
		},
		ResourceKeys: resourceKeys,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrImageUpdateRejected, err)
	}
	planHash, err := plan.Hash()
	if err != nil {
		return nil, err
	}
	operationID, err := randomManifestUpdateToken()
	if err != nil {
		return nil, err
	}
	record := &TransitionRecord{
		SchemaVersion: TransitionPlanSchemaVersion,
		OperationID:   operationID,
		InstanceID:    instanceID,
		Phase:         TransitionPhasePrepared,
		PlanHash:      planHash,
		Plan:          *plan,
	}
	if err := state.StoreTransitionRecord(instanceID, record); err != nil {
		return nil, fmt.Errorf("store image update transition record: %w", err)
	}
	return record, nil
}

func rejectImageUpdateIfLegacyJournalPending(state *FilesystemStateManager, instanceID string) error {
	if existing, err := state.LoadImageUpdateTransaction(instanceID); err == nil && existing != nil {
		return fmt.Errorf("%w: image update already has pending rollback state in phase %s", ErrImageUpdateRejected, existing.Phase)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: image update rollback state unreadable: %v", ErrImageUpdateRejected, err)
	}
	if existing, err := state.LoadManifestUpdateTransaction(instanceID); err == nil && existing != nil {
		return fmt.Errorf("%w: manifest update transaction already in progress (phase %s)", ErrImageUpdateRejected, existing.Phase)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: manifest update rollback state unreadable: %v", ErrImageUpdateRejected, err)
	}
	return nil
}

func (m *AppManager) resolveUpdateImageRootfsPlan(ctx context.Context, instanceID string, appInst *AppInstance, updatedImages map[string]string) ([]ManifestUpdateImagePlanItem, error) {
	if len(updatedImages) == 0 {
		return nil, nil
	}
	if m.containerManager == nil {
		return nil, fmt.Errorf("container manager not configured")
	}
	ephRT, ephCleanup, err := m.newFlattenRuntime(ctx)
	if err != nil {
		return nil, fmt.Errorf("create ephemeral runtime: %w", err)
	}
	defer ephCleanup()
	rootfs := m.currentRootfsManager()
	if rootfs == nil {
		return nil, fmt.Errorf("rootfs volume manager not configured")
	}
	serviceNames := make([]string, 0, len(updatedImages))
	for svcName := range updatedImages {
		serviceNames = append(serviceNames, svcName)
	}
	slices.Sort(serviceNames)
	plan := make([]ManifestUpdateImagePlanItem, 0, len(serviceNames))
	for _, svcName := range serviceNames {
		imageRef := strings.TrimSpace(updatedImages[svcName])
		if imageRef == "" {
			continue
		}
		if err := m.containerManager.PullImage(ctx, ephRT, imageRef); err != nil {
			return nil, fmt.Errorf("pull image %s (service %s): %w", imageRef, svcName, err)
		}
		imgConfig, err := m.containerManager.InspectImage(ctx, ephRT, imageRef)
		if err != nil {
			return nil, fmt.Errorf("inspect image %s (service %s): %w", imageRef, svcName, err)
		}
		digest := imageConfigDigest(imgConfig)
		if digest == "" {
			return nil, fmt.Errorf("inspect image %s (service %s): digest unavailable", imageRef, svcName)
		}
		canonicalDigest := canonicalImageDigestKey(digest)
		if canonicalDigest == "" {
			return nil, fmt.Errorf("inspect image %s (service %s): canonical digest unavailable", imageRef, svcName)
		}
		previousRootfs := ""
		if appInst.ActiveRootfs != nil {
			previousRootfs = appInst.ActiveRootfs[svcName]
		}
		if matches, _, err := manifestUpdateRootfsProvesDigest(rootfs, previousRootfs, canonicalDigest); err == nil && matches {
			continue
		}
		plan = append(plan, ManifestUpdateImagePlanItem{
			ServiceName:            svcName,
			EntryKind:              manifestUpdateImageEntryAppService,
			Action:                 manifestUpdateImageActionRefresh,
			Reason:                 "current image digest differs from active rootfs",
			ImageRef:               imageRef,
			ResolvedDigest:         digest,
			CanonicalDigest:        canonicalDigest,
			RootfsVolumeID:         persistence.VersionedServiceRootfsVolumeID(instanceID, svcName, persistence.ShortDigest(canonicalDigest)),
			PreviousRootfsVolumeID: previousRootfs,
		})
	}
	return plan, nil
}

func (m *AppManager) ImageUpdateBlockedReason(ctx context.Context, instanceID string) string {
	state, err := m.ensureStateManager()
	if err != nil {
		return err.Error()
	}
	appInst, exists := state.GetApp(instanceID)
	if !exists {
		return fmt.Sprintf("app instance not found: %s", instanceID)
	}
	def, err := state.GetAppDefinition(instanceID)
	if err != nil {
		return fmt.Sprintf("read current manifest: %v", err)
	}
	mode := piccoloModeFromExtensions(def.Extensions)
	if mode != ModeService || !appHasPersistentStorage(def) {
		return ""
	}
	hasRefreshableImage := false
	for _, svc := range def.Services {
		if strings.TrimSpace(svc.Image) != "" && !isDigestPinned(svc.Image) {
			hasRefreshableImage = true
			break
		}
	}
	if !hasRefreshableImage {
		return ""
	}
	if appInst != nil && !appInst.Enabled {
		return ""
	}
	if err := m.preflightImageRefreshRollbackSnapshot(ctx, instanceID, def); err != nil {
		return err.Error()
	}
	return ""
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

type dataSnapshotViabilityChecker interface {
	CheckDataSnapshotViability(ctx context.Context, instanceID string) error
}

type dataSnapshotHealthChecker interface {
	CheckDataSnapshotHealth(ctx context.Context, snapshotLVName string) error
}

func (m *AppManager) preflightImageRefreshRollbackSnapshot(ctx context.Context, instanceID string, def *api.AppDefinition) error {
	if !appHasPersistentStorage(def) {
		return nil
	}
	volumeManager := m.currentVolumeManager()
	if _, ok := volumeManager.(dataVolumeSnapshotter); !ok {
		return fmt.Errorf("%w: image refresh rollback snapshot required but volume manager does not support snapshots", ErrImageUpdateRejected)
	}
	if checker, ok := volumeManager.(dataSnapshotViabilityChecker); ok {
		if err := checker.CheckDataSnapshotViability(ctx, instanceID); err != nil {
			return fmt.Errorf("%w: image refresh rollback snapshot viability: %v", ErrImageUpdateRejected, err)
		}
	}
	return nil
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
	imagePlan []ManifestUpdateImagePlanItem,
) (err error) {
	instanceID := appInst.InstanceID
	curDef := appInst.Definition
	primary := primaryServiceFor(newDef, appInst)
	mode := piccoloModeFromExtensions(newDef.Extensions)
	rollbackSnapshotRequired := appHasPersistentStorage(newDef)
	var imageTxn *ImageUpdateTransaction
	var plannedTupleState *TupleState

	rootfs := m.currentRootfsManager()
	if rootfs == nil {
		return fmt.Errorf("rootfs volume manager not configured")
	}
	transitionOperationID := ""
	if record, loadErr := state.LoadTransitionRecord(instanceID); loadErr == nil && record != nil {
		transitionOperationID = record.OperationID
	} else if loadErr != nil && !errors.Is(loadErr, os.ErrNotExist) {
		return fmt.Errorf("load image update transition record: %w", loadErr)
	}
	if rollbackSnapshotRequired {
		imageTxn, plannedTupleState, err = m.planImageUpdateRollbackTransaction(ctx, state, appInst, primary, transitionOperationID)
		if err != nil {
			return err
		}
		defer func() {
			if err == nil || imageTxn == nil || imageTxn.RuntimeSwitchStarted {
				return
			}
			if clearErr := m.clearImageUpdatePreCandidateAbort(ctx, state, instanceID, imageTxn); clearErr != nil {
				log.Printf("WARN: update %s: clear pre-switch image update transaction: %v", instanceID, clearErr)
				err = errors.Join(err, fmt.Errorf("clear pre-switch image update transaction: %w", clearErr))
			}
		}()
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
		canonical     string
		volumeID      string
		imageSizeHint int64
		handle        persistence.RootfsHandle
		imgConfig     persistence.GoldenImageConfig
	}
	changed := make([]changedService, 0, len(imagePlan))
	for _, planned := range imagePlan {
		svcName := strings.TrimSpace(planned.ServiceName)
		newImage := strings.TrimSpace(planned.ImageRef)
		if svcName == "" || newImage == "" {
			return fmt.Errorf("image update plan contains empty service or image reference")
		}
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
		canonicalDigest := canonicalImageDigestKey(digest)
		if canonicalDigest == "" {
			return fmt.Errorf("inspect image %s (service %s): canonical digest unavailable", newImage, svcName)
		}
		if expected := manifestUpdateImagePlanCanonicalDigest(planned); expected != "" && expected != canonicalDigest {
			return fmt.Errorf("%w: image digest changed during image update for service %s", ErrImageUpdateRejected, svcName)
		}
		volID := strings.TrimSpace(planned.RootfsVolumeID)
		expectedVolID := persistence.VersionedServiceRootfsVolumeID(instanceID, svcName, persistence.ShortDigest(canonicalDigest))
		if volID == "" {
			volID = expectedVolID
		}
		if volID != expectedVolID {
			return fmt.Errorf("%w: rootfs identity changed during image update for service %s", ErrImageUpdateRejected, svcName)
		}
		changed = append(changed, changedService{
			svcName:       svcName,
			newImage:      newImage,
			canonical:     canonicalDigest,
			volumeID:      volID,
			imageSizeHint: imgConfig.Size,
		})
	}

	plannedStagedRootfsIDs := transitionRootfsKeysFromManifestPlan(imagePlan)
	candidateActiveRootfs := cloneStringMap(appInst.ActiveRootfs)
	if candidateActiveRootfs == nil {
		candidateActiveRootfs = map[string]string{}
	}
	for _, cs := range changed {
		candidateActiveRootfs[cs.svcName] = cs.volumeID
	}
	if imageTxn != nil {
		imageTxn.StagedRootfs = imageUpdateStagedRootfsMap(plannedStagedRootfsIDs)
		imageTxn.CandidateActiveRootfs = cloneStringMap(candidateActiveRootfs)
		imageTxn.LastError = ""
		if storeErr := state.StoreImageUpdateTransaction(instanceID, imageTxn); storeErr != nil {
			return fmt.Errorf("store image update staged rootfs marker: %w", storeErr)
		}
		if storeErr := storeTransitionRecordForImageUpdate(state, instanceID, imageTxn, appInst); storeErr != nil {
			return fmt.Errorf("store image update transition staged rootfs marker: %w", storeErr)
		}
	} else if storeErr := storeTransitionRecordForImageUpdateNoJournal(state, instanceID, TransitionPhaseResourcesPrepared, plannedStagedRootfsIDs, nil, nil, nil); storeErr != nil {
		return fmt.Errorf("store image update transition staged rootfs marker: %w", storeErr)
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

	markCreatedRootfsForCleanup := func(volID string) error {
		volID = strings.TrimSpace(volID)
		if volID == "" {
			return nil
		}
		if imageTxn != nil {
			if imageTxn.CreatedRootfs == nil {
				imageTxn.CreatedRootfs = map[string]string{}
			}
			imageTxn.CreatedRootfs[volID] = volID
			imageTxn.LastError = ""
			if err := state.StoreImageUpdateTransaction(instanceID, imageTxn); err != nil {
				return fmt.Errorf("store image update created rootfs marker: %w", err)
			}
			if err := storeTransitionRecordForImageUpdate(state, instanceID, imageTxn, appInst); err != nil {
				return fmt.Errorf("store image update transition created rootfs marker: %w", err)
			}
			return nil
		}
		if err := storeTransitionRecordForImageUpdateNoJournal(state, instanceID, TransitionPhaseResourcesPrepared, plannedStagedRootfsIDs, []string{volID}, nil, nil); err != nil {
			return fmt.Errorf("store image update transition created rootfs marker: %w", err)
		}
		return nil
	}

	// 3. For each changed service: create new rootfs (idempotent, while still running).
	for i := range changed {
		cs := &changed[i]
		if rootfs.RootfsExists(cs.volumeID) {
			if err := verifyRootfsIdentityForDigest(rootfs, cs.volumeID, cs.canonical); err != nil {
				return fmt.Errorf("existing rootfs for service %s does not match planned image identity: %w", cs.svcName, err)
			}
			log.Printf("INFO: update %s: rootfs %s already exists, attaching", instanceID, cs.volumeID)
			cs.handle, err = rootfs.AttachRootfs(ctx, cs.volumeID)
			if err != nil {
				return fmt.Errorf("attach existing rootfs %s: %w", cs.volumeID, err)
			}
		} else {
			if err := markCreatedRootfsForCleanup(cs.volumeID); err != nil {
				return err
			}
			cs.handle, err = rootfs.CreateServiceRootfs(ctx, persistence.ServiceRootfsRequest{
				InstanceID:    instanceID,
				ServiceName:   cs.svcName,
				ImageDigest:   cs.canonical,
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
	stagedRootfsIDs := func() []string {
		out := make([]string, 0, len(changed))
		for _, cs := range changed {
			if appInst.ActiveRootfs != nil && appInst.ActiveRootfs[cs.svcName] == cs.volumeID {
				continue
			}
			out = append(out, cs.volumeID)
		}
		return out
	}
	detachStagedChangedRootfs := func() {
		for _, cs := range changed {
			if appInst.ActiveRootfs != nil && appInst.ActiveRootfs[cs.svcName] == cs.volumeID {
				continue
			}
			_ = rootfs.DetachRootfs(ctx, cs.volumeID)
		}
	}
	restartPreviousRuntime := func(cause error) error {
		detachStagedChangedRootfs()
		if restartErr := m.startContainerGroup(ctx, state, appInst, curDef, layout, runtime); restartErr != nil {
			m.setObservedStatus(instanceID, StatusError)
			return errors.Join(cause, fmt.Errorf("restart previous runtime: %w", restartErr))
		}
		m.setObservedStatus(instanceID, StatusRunning)
		if imageTxn != nil && !imageTxn.CandidateDataRisk && !imageTxn.CommitIntent {
			if clearErr := m.clearImageUpdatePreCandidateAbort(ctx, state, instanceID, imageTxn); clearErr != nil {
				return errors.Join(cause, fmt.Errorf("clear pre-candidate image update transaction: %w", clearErr))
			}
		}
		return cause
	}

	// 4. Read image config from golden LV for each changed service.
	for i := range changed {
		cs := &changed[i]
		goldenCfg, cfgErr := m.readImageConfigForGoldenRootfs(ctx, rootfs, cs.handle.GoldenLV, cs.canonical)
		if cfgErr != nil {
			log.Printf("WARN: update %s: failed to read image config for %s: %v", instanceID, cs.svcName, cfgErr)
		} else {
			cs.imgConfig = goldenCfg
		}
	}
	if rollbackSnapshotRequired {
		if checker, ok := m.currentVolumeManager().(dataSnapshotViabilityChecker); ok {
			if err := checker.CheckDataSnapshotViability(ctx, instanceID); err != nil {
				detachStagedChangedRootfs()
				return fmt.Errorf("%w: image refresh rollback snapshot viability: %v", ErrImageUpdateRejected, err)
			}
		}
	}

	m.emitProgress(ctx, taskTypeUpdateImage, instanceID, taskPhaseStopping, 50, "Stopping containers", false, nil)

	// 5. Prove the app quiescent before creating any data snapshot or
	// detaching/replacing rootfs. A failed proof leaves the existing runtime and
	// storage untouched.
	if err := m.quiesceContainerGroup(ctx, state, appInst, curDef, layout); err != nil {
		return fmt.Errorf("quiesce before image update: %w", err)
	}
	// PID 1 fallback may have stopped the user manager. Reacquire it for the
	// later Podman cleanup/recreate commands; no workload is started here.
	runtime, err = m.podmanRuntimeForApp(ctx, instanceID, layout, mode, appRuntimeEnsureReady)
	if err != nil {
		return fmt.Errorf("reacquire runtime after image-update quiesce: %w", err)
	}
	if imageTxn != nil {
		imageTxn.Phase = imageUpdatePhaseRuntimeSwitch
		imageTxn.RuntimeSwitchStarted = true
		imageTxn.LastError = ""
		if storeErr := state.StoreImageUpdateTransaction(instanceID, imageTxn); storeErr != nil {
			return restartPreviousRuntime(fmt.Errorf("store image update runtime switch marker: %w", storeErr))
		}
		if storeErr := storeTransitionRecordForImageUpdate(state, instanceID, imageTxn, appInst); storeErr != nil {
			return restartPreviousRuntime(fmt.Errorf("store image update transition runtime switch marker: %w", storeErr))
		}
	} else if storeErr := storeTransitionRecordForImageUpdateNoJournal(state, instanceID, TransitionPhaseSwitchingRuntime, stagedRootfsIDs(), nil, nil, nil); storeErr != nil {
		return restartPreviousRuntime(fmt.Errorf("store image update transition runtime switch marker: %w", storeErr))
	}

	var tupleState *TupleState
	snapshotOK := false
	if rollbackSnapshotRequired {
		m.emitProgress(ctx, taskTypeUpdateImage, instanceID, taskPhaseSnapshotting, 55, "Creating rollback snapshot", false, nil)

		// 6. Tuple snapshot: capture pre-update state for rollback.
		var snapshotErr error
		tupleState, snapshotOK, snapshotErr = m.snapshotTupleBeforeUpdate(ctx, state, appInst, primary, imageTxn, plannedTupleState)
		if !snapshotOK {
			if snapshotErr == nil {
				snapshotErr = fmt.Errorf("snapshot was not created")
			}
			err := fmt.Errorf("create rollback snapshot for image refresh: %w", snapshotErr)
			log.Printf("WARN: update %s: %v", instanceID, err)
			return restartPreviousRuntime(err)
		}
	}

	// Build rootfs map: changed services use new rootfs, unchanged use existing.
	changedMap := make(map[string]*rootfsMountInfo, len(changed))
	for i := range changed {
		cs := &changed[i]
		changedMap[cs.svcName] = &rootfsMountInfo{handle: cs.handle, imgConfig: cs.imgConfig}
	}

	// Attach unchanged service rootfs volumes.
	unchangedRootfs, err := m.ensureAllServiceRootfsAttached(ctx, instanceID, mode, newDef, appInst)
	if err != nil {
		return restartPreviousRuntime(fmt.Errorf("update %s: attach unchanged rootfs: %w", instanceID, err))
	}

	// Merge: changed services override unchanged.
	prebuiltRootfs := make(map[string]*rootfsMountInfo, len(newDef.Services))
	for svcName, info := range unchangedRootfs {
		prebuiltRootfs[svcName] = info
	}
	for svcName, info := range changedMap {
		prebuiltRootfs[svcName] = info
	}
	if imageTxn != nil {
		imageTxn.Phase = imageUpdatePhaseCandidateDataRisk
		imageTxn.CandidateDataRisk = true
		imageTxn.CandidateActiveRootfs = cloneStringMap(candidateActiveRootfs)
		imageTxn.LastError = ""
		if storeErr := state.StoreImageUpdateTransaction(instanceID, imageTxn); storeErr != nil {
			return restartPreviousRuntime(fmt.Errorf("store image update candidate data risk marker: %w", storeErr))
		}
		if storeErr := storeTransitionRecordForImageUpdate(state, instanceID, imageTxn, appInst); storeErr != nil {
			return restartPreviousRuntime(fmt.Errorf("store image update transition candidate data risk marker: %w", storeErr))
		}
		imageTxn.Phase = imageUpdatePhaseCommitIntent
		imageTxn.CommitIntent = true
		if storeErr := state.StoreImageUpdateTransaction(instanceID, imageTxn); storeErr != nil {
			detachStagedChangedRootfs()
			m.setObservedStatus(instanceID, StatusError)
			return fmt.Errorf("store image update commit intent after candidate data risk marker: %w", storeErr)
		}
		if storeErr := storeTransitionRecordForImageUpdate(state, instanceID, imageTxn, appInst); storeErr != nil {
			detachStagedChangedRootfs()
			m.setObservedStatus(instanceID, StatusError)
			return fmt.Errorf("store image update transition commit intent marker: %w", storeErr)
		}
	}

	m.emitProgress(ctx, taskTypeUpdateImage, instanceID, taskPhaseRemovingContainer, 60, "Removing containers", false, nil)

	// 7. Remove ALL containers (services + anchor).
	if err := m.removeContainersForMultiApp(ctx, appInst, curDef, runtime); err != nil {
		log.Printf("WARN: update %s: remove containers: %v", instanceID, err)
	}

	m.emitProgress(ctx, taskTypeUpdateImage, instanceID, taskPhaseRecreatingContainer, 70, "Recreating containers", false, nil)

	// 8. Recreate ALL containers (anchor + services in start order).

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
	result, err := m.installContainerGroup(ctx, newDef, instanceID, layout, runtime, endpoints, prebuiltRootfs, true, false)
	if err != nil {
		if uncommittedContainerGroupMaySurvive(err) {
			m.setObservedStatus(instanceID, StatusError)
			return fmt.Errorf("recreate containers after update: %w", err)
		}
		// Detach new rootfs volumes, leave old ones for recovery.
		for _, cs := range changed {
			_ = rootfs.DetachRootfs(ctx, cs.volumeID)
		}
		m.setObservedStatus(instanceID, StatusError)
		return fmt.Errorf("recreate containers after update: %w", err)
	}
	if imageTxn == nil {
		if storeErr := storeTransitionRecordForImageUpdateNoJournal(state, instanceID, TransitionPhaseCommitIntent, stagedRootfsIDs(), nil, result, nil); storeErr != nil {
			cause := fmt.Errorf("store image update transition commit intent marker: %w", storeErr)
			if cleanupErr := m.compensateUncommittedContainerGroup(
				state,
				appInst,
				result,
				newDef,
				runtime,
			); cleanupErr != nil {
				m.setObservedStatus(instanceID, StatusError)
				return errors.Join(
					cause,
					fmt.Errorf("compensate uncommitted container group: %w", cleanupErr),
				)
			}
			return restartPreviousRuntime(cause)
		}
	}

	m.emitProgress(ctx, taskTypeUpdateImage, instanceID, taskPhaseFinalizing, 90, "Saving state", false, nil)

	// 9. Update state: ActiveRootfs for changed services, definition, container IDs.
	candidate, err := recreatedAppCandidate(appInst, result)
	if err != nil {
		return m.abortUncommittedContainerGroup(err, state, appInst, result, newDef, runtime)
	}
	candidate.ActiveRootfs = cloneStringMap(appInst.ActiveRootfs)
	if candidate.ActiveRootfs == nil {
		candidate.ActiveRootfs = make(map[string]string)
	}
	for _, cs := range changed {
		candidate.ActiveRootfs[cs.svcName] = cs.volumeID
	}
	oldArtifactReferences := cloneStringMap(appInst.ArtifactReferences)
	candidate.Definition = newDef
	candidate.UpdatedAt = time.Now()
	if imageTxn != nil {
		imageTxn.CandidatePrimaryService = result.PrimaryService
		imageTxn.CandidateNetworkAnchorID = result.NetworkAnchorID
		imageTxn.CandidateContainers = cloneStringMap(result.Containers)
		_ = state.StoreImageUpdateTransaction(instanceID, imageTxn)
		_ = storeTransitionRecordForImageUpdate(state, instanceID, imageTxn, candidate)
	}
	if err := commitDetachedApp(state, appInst, candidate); err != nil {
		commitErr := fmt.Errorf("store app: %w", err)
		var markerErr error
		if imageTxn == nil {
			if storeErr := storeTransitionRecordForImageUpdateNoJournal(state, instanceID, TransitionPhaseRestoringPrevious, stagedRootfsIDs(), nil, nil, err); storeErr != nil {
				markerErr = fmt.Errorf("store image update transition restore marker: %w", storeErr)
			}
		}
		if markerErr != nil {
			// The durable commit-intent record still owns the live candidate.
			// Removing it without durably advancing to restoring_previous would
			// make recovery's recorded runtime identity false.
			m.setObservedStatus(instanceID, StatusError)
			return errors.Join(commitErr, markerErr)
		}
		abortCause := errors.Join(commitErr, markerErr)
		if cleanupErr := m.compensateUncommittedContainerGroup(
			state,
			appInst,
			result,
			newDef,
			runtime,
		); cleanupErr != nil {
			m.setObservedStatus(instanceID, StatusError)
			return errors.Join(
				abortCause,
				fmt.Errorf("compensate uncommitted container group: %w", cleanupErr),
			)
		}
		if imageTxn == nil {
			rollbackErr := restartPreviousRuntime(abortCause)
			if clearErr := state.ClearTransitionRecord(instanceID); clearErr != nil {
				rollbackErr = errors.Join(rollbackErr, fmt.Errorf("clear aborted image transition: %w", clearErr))
			}
			return rollbackErr
		}
		m.setObservedStatus(instanceID, StatusError)
		return abortCause
	}
	m.releaseSupersededArtifactReferences(ctx, oldArtifactReferences, appInst.ArtifactReferences)

	// 10. Post-update: record new active generation (only when snapshot exists for rollback).
	if tupleState != nil && snapshotOK {
		if err := m.recordPostUpdateGeneration(state, appInst, tupleState); err != nil {
			if imageTxn != nil {
				imageTxn.Phase = imageUpdatePhaseCommitIntent
				imageTxn.CommitIntent = true
				imageTxn.LastError = fmt.Sprintf("record post-update generation: %v", err)
				_ = state.StoreImageUpdateTransaction(instanceID, imageTxn)
				_ = storeTransitionRecordForImageUpdate(state, instanceID, imageTxn, appInst)
			}
			m.setObservedStatus(instanceID, StatusError)
			return fmt.Errorf("record post-update generation: %w", err)
		}
	}
	if imageTxn != nil {
		imageTxn.Phase = imageUpdatePhaseCommitted
		imageTxn.LastError = ""
		if storeErr := state.StoreImageUpdateTransaction(instanceID, imageTxn); storeErr != nil {
			log.Printf("WARN: update %s: mark image update committed: %v", instanceID, storeErr)
		} else if storeErr := storeTransitionRecordForImageUpdate(state, instanceID, imageTxn, appInst); storeErr != nil {
			log.Printf("WARN: update %s: mark image transition committed: %v", instanceID, storeErr)
		} else if clearErr := state.ClearImageUpdateTransaction(instanceID); clearErr != nil {
			log.Printf("WARN: update %s: clear committed image update transaction: %v", instanceID, clearErr)
		}
	}

	m.setObservedStatus(instanceID, StatusRunning)
	log.Printf("INFO: update %s: image updated for %d service(s)", instanceID, len(changed))
	return nil
}

// snapshotTupleBeforeUpdate captures pre-update state for rollback (RFC 20260302 Phase 2).
// Returns the TupleState and whether the snapshot was successfully created.
func (m *AppManager) snapshotTupleBeforeUpdate(
	ctx context.Context, state *FilesystemStateManager,
	appInst *AppInstance, primary string,
	imageTxn *ImageUpdateTransaction, plannedState *TupleState,
) (tupleState *TupleState, snapshotOK bool, err error) {
	instanceID := appInst.InstanceID
	if imageTxn == nil {
		return nil, false, fmt.Errorf("image update transaction required")
	}

	// Load or create TupleState.
	ts := plannedState
	if ts == nil {
		ts, err = state.LoadTupleState(instanceID)
		if err != nil {
			log.Printf("WARN: update %s: load tuple state: %v", instanceID, err)
			return nil, false, fmt.Errorf("load tuple state: %w", err)
		}
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

	genID := imageTxn.SnapshotGenerationID
	if strings.TrimSpace(genID) == "" {
		genID = fmt.Sprintf("gen-%d", imageTxn.SnapshotGenerationNumber)
	}
	snapshotLVName := strings.TrimSpace(imageTxn.SnapshotLVName)
	if snapshotLVName == "" {
		snapshotLVName = DataSnapshotLVName(instanceID, imageTxn.SnapshotGenerationNumber)
	}

	// Capture current rootfs state.
	rootfsVolIDs := cloneStringMap(imageTxn.PreviousActiveRootfs)
	if appInst.ActiveRootfs != nil {
		rootfsVolIDs = make(map[string]string)
		for k, v := range appInst.ActiveRootfs {
			rootfsVolIDs[k] = v
		}
	} else if primary != "" {
		// Legacy fallback.
		rootfsVolIDs[primary] = persistence.ServiceRootfsVolumeID(instanceID, primary)
	}

	// Snapshot data volume.
	dataSnapshotName := ""
	if snapshotter, ok := m.currentVolumeManager().(dataVolumeSnapshotter); ok {
		if snapErr := snapshotter.SnapshotDataVolume(ctx, instanceID, snapshotLVName); snapErr != nil {
			log.Printf("WARN: update %s: data snapshot failed: %v", instanceID, snapErr)
			return ts, false, fmt.Errorf("create data snapshot: %w", snapErr)
		} else {
			dataSnapshotName = snapshotLVName
			snapshotOK = true
			log.Printf("INFO: update %s: data snapshot created: %s", instanceID, snapshotLVName)
			imageTxn.Phase = imageUpdatePhaseSnapshotCreated
			imageTxn.LastError = ""
			if storeErr := state.StoreImageUpdateTransaction(instanceID, imageTxn); storeErr != nil {
				log.Printf("WARN: update %s: mark image update snapshot created: %v", instanceID, storeErr)
				return ts, false, fmt.Errorf("mark image update snapshot created: %w", storeErr)
			}
			if storeErr := storeTransitionRecordForImageUpdate(state, instanceID, imageTxn, appInst); storeErr != nil {
				return ts, false, fmt.Errorf("mark image transition snapshot created: %w", storeErr)
			}
		}
	} else {
		log.Printf("WARN: update %s: volume manager does not support snapshots", instanceID)
		return ts, false, fmt.Errorf("volume manager does not support snapshots")
	}

	// Only create a rollback-capable snapshot generation if the data snapshot succeeded.
	// Without a data snapshot, rootfs-only rollback would run old rootfs against new data.
	if dataSnapshotName == "" {
		log.Printf("WARN: update %s: no data snapshot", instanceID)
		return ts, false, fmt.Errorf("data snapshot was not created")
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
		var cleanupErr error
		if dataSnapshotName != "" {
			if cleanupSnap, ok := m.currentVolumeManager().(dataVolumeSnapshotter); ok {
				if destroyErr := cleanupSnap.DestroyDataSnapshot(ctx, dataSnapshotName); destroyErr != nil {
					log.Printf("WARN: update %s: cleanup orphaned snapshot %s: %v", instanceID, dataSnapshotName, destroyErr)
					cleanupErr = fmt.Errorf("cleanup orphaned snapshot %s: %w", dataSnapshotName, destroyErr)
				}
			}
		}
		snapshotOK = false
		if cleanupErr != nil {
			return ts, false, errors.Join(fmt.Errorf("persist tuple state: %w", storeErr), cleanupErr)
		}
		if imageTxn != nil {
			if clearErr := state.ClearTransitionRecord(instanceID); clearErr != nil {
				return ts, false, errors.Join(fmt.Errorf("persist tuple state: %w", storeErr), fmt.Errorf("clear image transition after snapshot cleanup: %w", clearErr))
			}
			if clearErr := state.ClearImageUpdateTransaction(instanceID); clearErr != nil {
				return ts, false, errors.Join(fmt.Errorf("persist tuple state: %w", storeErr), fmt.Errorf("clear image update transaction after snapshot cleanup: %w", clearErr))
			}
		}
		return ts, false, fmt.Errorf("persist tuple state: %w", storeErr)
	}

	return ts, snapshotOK, nil
}

// recordPostUpdateGeneration records the new active generation after a successful update.
func (m *AppManager) recordPostUpdateGeneration(state *FilesystemStateManager, appInst *AppInstance, ts *TupleState) error {
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
		return fmt.Errorf("persist post-update generation: %w", err)
	}
	return nil
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
	defer pressure.BeginLifecycleOwner("app:" + instanceID)()
	if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkLifecycle); err != nil {
		return err
	}
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()
	state, err := m.ensureStateManager()
	if err != nil {
		return err
	}
	if err := m.ensureUnlocked(); err != nil {
		return err
	}
	if err := m.ensureKernelLeader(); err != nil {
		return err
	}
	if err := m.recoverPendingImageUpdateBeforeTransitionFence(ctx, state, instanceID, "rollback"); err != nil {
		return err
	}
	if err := m.rejectIfTransitionInProgress(state, instanceID, TransitionFenceRollback); err != nil {
		return err
	}
	appInst, exists := state.GetApp(instanceID)
	if !exists {
		return fmt.Errorf("app instance not found: %s", instanceID)
	}
	if recovered, recoverErr := m.recoverPendingImageUpdateForApp(ctx, state, appInst); recoverErr != nil {
		m.setObservedStatus(instanceID, StatusError)
		return fmt.Errorf("recover pending image update before rollback: %w", recoverErr)
	} else if recovered {
		var ok bool
		appInst, ok = state.GetApp(instanceID)
		if !ok {
			return fmt.Errorf("app instance not found after image update recovery: %s", instanceID)
		}
	}
	return m.rollbackToSnapshotLocked(ctx, state, appInst)
}

// HasSnapshotAvailable returns whether a rollback snapshot exists for the given app instance.
func (m *AppManager) HasSnapshotAvailable(ctx context.Context, instanceID string) bool {
	state, err := m.ensureStateManager()
	if err != nil {
		return false
	}
	if txn, err := state.LoadImageUpdateTransaction(instanceID); err == nil && txn != nil {
		return false
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
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
	if recovered, recoverErr := m.reconcilePartialRollback(ctx, state, appInst); recoverErr != nil {
		return fmt.Errorf("recover pending rollback: %w", recoverErr)
	} else if recovered {
		return m.reconcileApp(ctx, state, appInst)
	}

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
	// 1. Prove process absence before any data LV rename or rootfs detach.
	if err := m.quiesceContainerGroup(ctx, state, appInst, curDef, layout); err != nil {
		return fmt.Errorf("quiesce before rollback: %w", err)
	}

	m.emitProgress(ctx, taskTypeRollbackApp, instanceID, taskPhaseSnapshotting, 30, "Restoring data volume", false, nil)

	// 2. Rollback data volume if snapshot has one.
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
			if parsed, ok := parseTupleGenerationNumber(activeGen.ID); ok {
				failedGenNumber = parsed
			} else {
				log.Printf("WARN: rollback %s: could not parse gen number from %q, using fallback %d",
					instanceID, activeGen.ID, failedGenNumber)
			}
		}
		failedLVName := ""
		trackingGen := activeGen
		if trackingGen == nil {
			if current := ts.GenerationByID(ts.CurrentGeneration); current != nil && current.Status == TupleStatusFailed {
				trackingGen = current
			}
		}
		if trackingGen != nil && trackingGen.Status == TupleStatusFailed {
			failedLVName = strings.TrimSpace(trackingGen.FailedLVName)
		}
		if failedLVName == "" {
			failedLVName, err = m.allocateFailedDataLVName(ctx, state, instanceID, failedGenNumber, ts)
			if err != nil {
				return fmt.Errorf("allocate failed data LV name: %w", err)
			}
		}
		// Persist the intended LV swap before the first irreversible rename.
		// If the daemon exits anywhere inside RollbackDataVolume or before the
		// promoted tuple write, startup recovery can safely invoke the idempotent
		// rollback primitive with the same names.
		snap.RollbackPending = true
		snap.RollbackFailedLVName = failedLVName
		if err := state.StoreTupleState(instanceID, ts); err != nil {
			return fmt.Errorf("persist rollback LV intent: %w", err)
		}

		renamesCommitted, snapshotPromoted, rollbackErr := rollbacker.RollbackDataVolume(ctx, instanceID, snap.DataSnapshot, failedLVName)
		if rollbackErr != nil && !renamesCommitted {
			// Pre-swap failure (detach or rename). No LV state change — safe to abort.
			// Containers were stopped in step 1 but no LV state changed, so the reconciler
			// will detect stopped containers and restart them on the next pass.
			log.Printf("ERROR: rollback %s: data volume rollback failed (no LV change): %v", instanceID, rollbackErr)
			snap.RollbackPending = false
			snap.RollbackFailedLVName = ""
			if storeErr := state.StoreTupleState(instanceID, ts); storeErr != nil {
				return errors.Join(fmt.Errorf("data volume rollback: %w", rollbackErr), fmt.Errorf("clear rollback LV intent: %w", storeErr))
			}
			return fmt.Errorf("data volume rollback: %w", rollbackErr)
		}
		if rollbackErr != nil {
			log.Printf("ERROR: rollback %s: LV rename(s) committed but incomplete: %v", instanceID, rollbackErr)
		}
		if !snapshotPromoted {
			// Partial rename: active→failed succeeded but snapshot→active failed.
			// Snapshot LV still exists under its original name — do NOT mark as promoted.
			snapshotCanPromote = false
		}

		// Record failed LV name for GC tracking.
		if renamesCommitted {
			recordFailedRollbackLV(ts, trackingGen, failedLVName, failedGenNumber)
		}
	}

	// 3. Commit generation and app truth immediately after the irreversible LV
	// rename boundary. Runtime/session reacquisition is deliberately later: a
	// failed user-manager repair must not leave durable metadata describing the
	// pre-rename world.
	// Re-resolve snap pointer because failed-generation tracking may have
	// reallocated the slice.
	snap = ts.LatestSnapshot()
	if snap == nil {
		return fmt.Errorf("rollback %s: snapshot generation lost after slice mutation", instanceID)
	}
	if active := ts.ActiveGeneration(); active != nil {
		active.Status = TupleStatusFailed
		failedNow := time.Now()
		active.FailedAt = &failedNow
	}
	if snapshotCanPromote {
		snap.Status = TupleStatusActive
		snap.DataSnapshot = "" // promoted — no longer a snapshot LV
		snap.RollbackAttempted = true
		snap.RollbackPending = false
		snap.RollbackFailedLVName = ""
		appStateCommitted := false
		snap.RollbackAppStateCommitted = &appStateCommitted
		ts.CurrentGeneration = snap.ID
	}

	if err := state.StoreTupleState(instanceID, ts); err != nil {
		m.setObservedStatus(instanceID, StatusError)
		return fmt.Errorf("persist tuple state after rollback LV commit: %w", err)
	}
	if snapshotCanPromote {
		if err := m.commitRollbackAppState(state, appInst, ts, snap); err != nil {
			m.setObservedStatus(instanceID, StatusError)
			return fmt.Errorf("commit app state during rollback: %w", err)
		}
		curDef = appInst.Definition
		mode = piccoloModeFromExtensions(curDef.Extensions)
	} else {
		m.setObservedStatus(instanceID, StatusError)
		return fmt.Errorf("rollback %s: active LV renamed but snapshot promotion is incomplete; durable tuple state retained for retry", instanceID)
	}

	// 4. Rollback may have replaced the active data LV, and PID 1 fallback may
	// have stopped the user manager. Resolve both only after durable truth is
	// committed, then use them for Podman cleanup/recreation.
	layout, err = m.ensureAppVolumeLayout(ctx, instanceID)
	if err != nil {
		m.setObservedStatus(instanceID, StatusError)
		return fmt.Errorf("resolve layout after rollback: %w", err)
	}
	// A promoted-but-unattached LV is recoverable: successful layout resolution
	// above proves the attach retry completed, so runtime recreation may proceed.
	runtime, err := m.podmanRuntimeForApp(ctx, instanceID, layout, mode, appRuntimeEnsureReady)
	if err != nil {
		m.setObservedStatus(instanceID, StatusError)
		return fmt.Errorf("reacquire runtime after rollback: %w", err)
	}

	// 5. Capture endpoints for port reuse (keep service registry intact for proxies).
	endpoints, _ := m.serviceManager.GetByApp(instanceID)

	// 6. Remove ALL containers (but preserve service registry — proxies stay running).
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

	// 8. Attach snapshot rootfs and recreate containers.
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
	result, err := m.installContainerGroup(ctx, curDef, instanceID, layout, runtime, endpoints, prebuiltRootfs, true, false)
	if err != nil {
		m.setObservedStatus(instanceID, StatusError)
		return fmt.Errorf("recreate containers after rollback: %w", err)
	}

	// 9. Publish the recreated generation only after its metadata is durable.
	candidate, err := recreatedAppCandidate(appInst, result)
	if err != nil {
		return m.abortUncommittedContainerGroup(err, state, appInst, result, curDef, runtime)
	}
	if err := commitDetachedApp(state, appInst, candidate); err != nil {
		m.setObservedStatus(instanceID, StatusError)
		return m.abortUncommittedContainerGroup(
			fmt.Errorf("store app after rollback: %w", err),
			state,
			appInst,
			result,
			curDef,
			runtime,
		)
	}

	m.setObservedStatus(instanceID, StatusRunning)
	log.Printf("INFO: rollback %s: rolled back to generation %s", instanceID, snap.ID)
	return nil
}

// UpdateListeners updates an app instance's listeners and recreates the container if necessary.
// ctx must NOT be derived from an HTTP request context — callers must use a server-scoped
// context so that the internal rollback closure (which captures ctx) survives connection drops.
func (m *AppManager) UpdateListeners(ctx context.Context, instanceID string, listeners []api.AppListener) (*api.AppDefinition, error) {
	defer pressure.BeginLifecycleOwner("app:" + instanceID)()
	if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkLifecycle); err != nil {
		return nil, err
	}
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()
	state, err := m.ensureStateManager()
	if err != nil {
		return nil, err
	}
	if err := m.rejectIfTransitionInProgress(state, instanceID, TransitionFenceListenerUpdate); err != nil {
		return nil, err
	}
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

	if !listenerCapabilityProvidersEqual(curDef.Listeners, listeners) {
		return nil, fmt.Errorf(
			"invalid listener configuration: capability provider declarations can only be changed through manifest update review",
		)
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

	layout, err := m.ensureAppVolumeLayout(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	runtime, err := m.podmanRuntimeForApp(ctx, instanceID, layout, mode, appRuntimeEnsureReady)
	if err != nil {
		return nil, err
	}

	// Backup current definition
	if err := state.BackupCurrentAppDefinition(instanceID); err != nil {
		return nil, fmt.Errorf("backup app.yaml: %w", err)
	}
	previousArtifactReferences := cloneStringMap(appInst.ArtifactReferences)
	candidate, err := detachedAppCandidate(appInst)
	if err != nil {
		return nil, err
	}
	var installedCandidate *AppInstance

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

		// Reuse the lifecycle quiescence boundary before replacing the wrapper.
		// If proof fails, restore the old listener allocation and leave runtime
		// storage untouched.
		if quiesceErr := m.quiesceContainerGroup(ctx, state, appInst, curDef, layout); quiesceErr != nil {
			_, _, rollbackErr := m.serviceManager.Reconcile(instanceID, curDef.Listeners)
			if rollbackErr != nil {
				return nil, errors.Join(fmt.Errorf("quiesce before listener update: %w", quiesceErr), fmt.Errorf("restore listener allocation: %w", rollbackErr))
			}
			return nil, fmt.Errorf("quiesce before listener update: %w", quiesceErr)
		}
		runtime, err = m.podmanRuntimeForApp(ctx, instanceID, layout, mode, appRuntimeEnsureReady)
		if err != nil {
			return nil, fmt.Errorf("reacquire runtime after listener quiesce: %w", err)
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
			rbInst, rbInstErr := m.installContainerGroup(ctx, curDef, instanceID, layout, runtime, rbResult.Endpoints, rbRootfs, true, false)
			if rbInstErr != nil {
				m.setObservedStatus(instanceID, StatusError)
				_ = state.StoreApp(appInst)
				return nil, fmt.Errorf("update failed: %w; rollback failed (install): %v", cause, rbInstErr)
			}
			rbCandidate, candidateErr := recreatedAppCandidate(appInst, rbInst)
			if candidateErr != nil {
				m.setObservedStatus(instanceID, StatusError)
				return nil, errors.Join(
					cause,
					m.abortUncommittedContainerGroup(
						candidateErr,
						state,
						appInst,
						rbInst,
						curDef,
						runtime,
					),
				)
			}
			rbCandidate.Definition = curDef
			if saveErr := commitDetachedApp(state, appInst, rbCandidate); saveErr != nil {
				m.setObservedStatus(instanceID, StatusError)
				rollbackPersistErr := m.abortUncommittedContainerGroup(
					fmt.Errorf("rollback persistence failed: %w", saveErr),
					state,
					appInst,
					rbInst,
					curDef,
					runtime,
				)
				return nil, errors.Join(cause, rollbackPersistErr)
			}
			m.setObservedStatus(instanceID, StatusRunning)
			return nil, fmt.Errorf("update failed: %w (rolled back to previous state)", cause)
		}

		// Attach all service rootfs volumes (idempotent — returns cached handle if mounted).
		prebuiltRootfs, rErr := m.ensureAllServiceRootfsAttached(ctx, instanceID, mode, &newDef, appInst)
		if rErr != nil {
			return rollbackContainers(fmt.Errorf("attach rootfs: %w", rErr))
		}

		// Recreate the entire container group (anchor + services) with updated endpoints.
		m.emitProgress(ctx, taskTypeUpdateListeners, instanceID, taskPhaseStarting, 70, "Starting containers", false, nil)
		newInst, installErr := m.installContainerGroup(ctx, &newDef, instanceID, layout, runtime, result.Endpoints, prebuiltRootfs, true, false)
		if installErr != nil {
			if uncommittedContainerGroupMaySurvive(installErr) {
				m.setObservedStatus(instanceID, StatusError)
				return nil, fmt.Errorf("recreate containers: %w", installErr)
			}
			return rollbackContainers(fmt.Errorf("recreate containers: %w", installErr))
		}
		installedCandidate = newInst

		candidate, err = recreatedAppCandidate(appInst, newInst)
		if err != nil {
			if cleanupErr := m.compensateUncommittedContainerGroup(
				state,
				appInst,
				newInst,
				&newDef,
				runtime,
			); cleanupErr != nil {
				return nil, errors.Join(
					err,
					fmt.Errorf("compensate uncommitted container group: %w", cleanupErr),
				)
			}
			return rollbackContainers(err)
		}
	}

	m.emitProgress(ctx, taskTypeUpdateListeners, instanceID, taskPhaseFinalizing, 90, "Saving configuration", false, nil)
	candidate.UpdatedAt = time.Now()
	candidate.Definition = &newDef
	if err := commitDetachedApp(state, appInst, candidate); err != nil {
		storeErr := fmt.Errorf("store app: %w", err)
		if needsRecreation {
			m.setObservedStatus(instanceID, StatusError)
			return nil, m.abortUncommittedContainerGroup(
				storeErr,
				state,
				appInst,
				installedCandidate,
				&newDef,
				runtime,
			)
		}
		return nil, storeErr
	}
	if needsRecreation {
		m.setObservedStatus(instanceID, StatusRunning)
	}
	m.releaseSupersededArtifactReferences(ctx, previousArtifactReferences, appInst.ArtifactReferences)

	return &newDef, nil
}

func listenerCapabilityProvidersEqual(oldListeners, newListeners []api.AppListener) bool {
	oldDef := &api.AppDefinition{Listeners: oldListeners}
	newDef := &api.AppDefinition{Listeners: newListeners}
	for _, capability := range registeredCapabilities() {
		oldListener, oldBasePath, oldProvided := providedCapability(oldDef, capability)
		newListener, newBasePath, newProvided := providedCapability(newDef, capability)
		if oldListener != newListener ||
			oldBasePath != newBasePath ||
			oldProvided != newProvided {
			return false
		}
	}
	return true
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
	runtime, err := m.podmanRuntimeForApp(ctx, instanceID, layout, mode, appRuntimeObserve)
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
	runtime, err := m.podmanRuntimeForApp(ctx, instanceID, layout, mode, appRuntimeObserve)
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
	if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkTerminal); err != nil {
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
	if err := m.rejectIfTransitionInProgress(state, instanceID, TransitionFenceShellExec); err != nil {
		return nil, err
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
	runtime, err := m.podmanRuntimeForApp(ctx, instanceID, layout, mode, appRuntimeObserve)
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
