package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"piccolod/internal/api"
	"piccolod/internal/app/workspacedisk"
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
	eventsMu         sync.Mutex
	eventCancel      context.CancelFunc
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

	// Workspace disk manager for container-independent persistence
	workspaceDiskMgr      *workspacedisk.DefaultManager
	workspacePathResolver *workspacePathResolver
	workspaceImageMounter *workspacedisk.PodmanImageMounter
}

var (
	ErrLocked            = errors.New("app manager: persistence locked")
	ErrNotLeader         = errors.New("app manager: not leader")
	ErrVolumeUnavailable = errors.New("app manager: persistence volume not mounted")
)

// LockStateReader exposes the control lock state.
type LockStateReader interface {
	ControlLocked() bool
}

const maxInstallPortRetries = 5

// workspaceSnapshotImage returns the local image name used to store workspace snapshots.
// Snapshots are committed when a workspace app is uninstalled without purge, preserving
// container filesystem changes for restoration on reinstall.
// The instanceID parameter is the unique instance identifier.
func workspaceSnapshotImage(instanceID string) string {
	return fmt.Sprintf("localhost/%s:snapshot", instanceID)
}

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

// workspaceRuntimeResolver implements workspacedisk.RuntimeResolver
// by looking up podman runtime configuration for app instances.
type workspaceRuntimeResolver struct {
	am *AppManager
}

func (r *workspaceRuntimeResolver) GetRuntimeArgs(ctx context.Context, instanceID string) ([]string, error) {
	// Ensure the volume is available (this might trigger attachment)
	layout, err := r.am.ensureAppVolumeLayout(ctx, instanceID)
	if err != nil {
		return nil, fmt.Errorf("ensure volume layout: %w", err)
	}

	// Get the podman runtime configuration
	runtime, err := r.am.podmanRuntimeForApp(instanceID, layout)
	if err != nil {
		return nil, fmt.Errorf("get runtime: %w", err)
	}

	// Convert configuration to command-line arguments
	args := []string{}
	if runtime.Root != "" {
		args = append(args, "--root", runtime.Root)
	}
	if runtime.RunRoot != "" {
		args = append(args, "--runroot", runtime.RunRoot)
	}
	if runtime.Imagestore != "" {
		args = append(args, "--imagestore", runtime.Imagestore)
	}
	if runtime.StorageDriver != "" {
		args = append(args, "--storage-driver", runtime.StorageDriver)
	}
	for _, opt := range runtime.StorageOpts {
		args = append(args, "--storage-opt", opt)
	}

	return args, nil
}

// NewAppManagerWithServices creates a new filesystem-based app manager with an injected ServiceManager
func NewAppManagerWithServices(containerManager ContainerManager, stateDir string, serviceManager *services.ServiceManager, lockReader LockStateReader) (*AppManager, error) {
	base := stateDir
	if strings.TrimSpace(base) == "" {
		base = paths.Root()
	}
	base = filepath.Clean(base)

	// Initialize workspace disk components
	pathResolver := newWorkspacePathResolver()
	imageMounter := workspacedisk.NewPodmanImageMounter()

	mgr := &AppManager{
		containerManager:      containerManager,
		stateBaseDir:          base,
		serviceManager:        serviceManager,
		leadershipState:       make(map[string]cluster.Role),
		lockReader:            lockReader,
		mountVerifier:         defaultMountVerifier,
		workspacePathResolver: pathResolver,
		workspaceImageMounter: imageMounter,
	}

	// Wire up runtime resolver and disk manager
	runtimeResolver := &workspaceRuntimeResolver{am: mgr}
	diskMgr := workspacedisk.NewManager(pathResolver, runtimeResolver, imageMounter)
	mgr.workspaceDiskMgr = diskMgr

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

// SetMountVerifier overrides the mount verification callback. Intended for tests.
func (m *AppManager) SetMountVerifier(fn func(string) error) {
	m.stateInitMu.Lock()
	m.mountVerifier = fn
	m.stateInitMu.Unlock()
}

// SetStateBaseDir overrides the base directory used for filesystem-backed state.
func (m *AppManager) SetStateBaseDir(dir string) {
	base := dir
	if strings.TrimSpace(base) == "" {
		base = paths.Root()
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

func (m *AppManager) emitProgress(ctx context.Context, taskType, instanceID, phase string, progress int, message string, complete bool, opErr error) {
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

	leaders := bus.Subscribe(events.TopicLeadershipRoleChanged, 16)
	locks := bus.Subscribe(events.TopicLockStateChanged, 8)
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
					go m.RestoreServices(loopCtx)
					go m.ReconcileOnce(loopCtx)
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
func (m *AppManager) StopRuntimeEvents() {
	m.eventsMu.Lock()
	if m.eventCancel != nil {
		m.eventCancel()
		m.eventCancel = nil
	}
	m.eventsMu.Unlock()
	m.eventsWG.Wait()
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
}

func (m *AppManager) StopBackground() {
	m.stateMu.Lock()
	cancel := m.reconcileCancel
	m.reconcileCancel = nil
	m.stateMu.Unlock()
	if cancel != nil {
		cancel()
	}
	m.reconcileWG.Wait()
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
		// Respect desired state: stopped apps should not have proxies restored.
		if app.Status == "stopped" {
			if m.serviceManager != nil {
				m.serviceManager.RemoveApp(app.InstanceID)
			}
			continue
		}
		// Followers should not restore proxies for apps they don't lead.
		if m.LastObservedRole(cluster.ResourceForApp(app.InstanceID)) == cluster.RoleFollower {
			if m.serviceManager != nil {
				m.serviceManager.RemoveApp(app.InstanceID)
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
		runtime, err := m.podmanRuntimeForApp(app.InstanceID, layout)
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
			m.serviceManager.RemoveApp(app.InstanceID)
			continue
		}
		if _, err := m.serviceManager.RestoreFromPodman(app.InstanceID, def.Listeners, ports); err != nil {
			log.Printf("WARN: restore services: failed to restore proxies for %s: %v", app.InstanceID, err)
			continue
		}
		m.serviceManager.SetAppContainerID(app.InstanceID, publishCID)
	}
}

// ReconcileOnce ensures Podman observed state converges to Piccolo desired state.
//
// Desired state is derived from app metadata:
// - status == "stopped" => desired stopped
// - any other status     => desired running
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

	for _, appInst := range state.ListApps() {
		if ctx.Err() != nil {
			return
		}
		if appInst == nil || strings.TrimSpace(appInst.InstanceID) == "" {
			continue
		}
		if err := m.reconcileApp(ctx, state, appInst); err != nil {
			log.Printf("WARN: reconcile app %s: %v", appInst.InstanceID, err)
		}
	}
}

func (m *AppManager) reconcileApp(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance) error {
	desiredRunning := appInst.Status != "stopped"
	if m.LastObservedRole(cluster.ResourceForApp(appInst.InstanceID)) == cluster.RoleFollower {
		desiredRunning = false
	}

	layout, err := m.ensureAppVolumeLayout(ctx, appInst.InstanceID)
	if err != nil {
		return err
	}
	runtime, err := m.podmanRuntimeForApp(appInst.InstanceID, layout)
	if err != nil {
		return err
	}

	def, err := state.GetAppDefinition(appInst.InstanceID)
	if err != nil {
		return err
	}
	if def.Services != nil {
		return m.reconcileMultiContainer(ctx, state, appInst, def, layout, runtime, desiredRunning)
	}

	containerID := strings.TrimSpace(appInst.ContainerID)
	resolveID := func() (string, error) {
		id, err := m.containerManager.ResolveContainerIDByName(ctx, runtime, appInst.InstanceID)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(id) == "" {
			return "", container.ErrContainerNotFound(appInst.InstanceID)
		}
		return id, nil
	}

	if containerID == "" {
		if id, err := resolveID(); err == nil {
			containerID = id
			_ = state.UpdateAppRuntime(appInst.InstanceID, appInst.Status, containerID)
			appInst.ContainerID = containerID
		}
	}

	var observed container.ContainerState
	if containerID != "" {
		observed, err = m.containerManager.InspectContainerState(ctx, runtime, containerID)
		if err != nil {
			log.Printf("WARN: reconcile app %s: inspect state failed: %v", appInst.InstanceID, err)
			observed = container.ContainerState{}
		}
	}

	if containerID == "" || !observed.Exists {
		if id, err := resolveID(); err == nil && id != "" && id != containerID {
			containerID = id
			_ = state.UpdateAppRuntime(appInst.InstanceID, appInst.Status, containerID)
			appInst.ContainerID = containerID
			observed, _ = m.containerManager.InspectContainerState(ctx, runtime, containerID)
		}
	}

	if containerID == "" || !observed.Exists {
		if !desiredRunning {
			if m.serviceManager != nil {
				m.serviceManager.RemoveApp(appInst.InstanceID)
			}
			return nil
		}
		def, err := state.GetAppDefinition(appInst.InstanceID)
		if err != nil {
			return err
		}
		return m.recreateMissingContainer(ctx, state, appInst, def, layout, runtime)
	}

	if !desiredRunning {
		if observed.Running {
			_ = m.containerManager.StopContainer(ctx, runtime, containerID)
		}
		if m.serviceManager != nil {
			m.serviceManager.RemoveApp(appInst.InstanceID)
		}
		return nil
	}

	// Desired running.
	if !observed.Running {
		if err := m.containerManager.StartContainer(ctx, runtime, containerID); err != nil {
			_ = state.UpdateAppStatus(appInst.InstanceID, "error")
			return err
		}
	}
	if appInst.Status != "running" {
		_ = state.UpdateAppStatus(appInst.InstanceID, "running")
	}

	if m.serviceManager != nil {
		if err := m.ensureServicesForRunningApp(ctx, def, appInst.InstanceID, containerID, runtime); err != nil {
			log.Printf("WARN: reconcile app %s: restore services failed: %v", appInst.InstanceID, err)
		}
		if err := m.ensurePodmanPublishes(ctx, def, appInst.InstanceID, containerID, runtime); err != nil {
			if errors.Is(err, container.ErrPortReconciliationRequired) {
				// Port bindings don't match and Podman doesn't support dynamic updates.
				// Recreate the container with the correct ports.
				log.Printf("INFO: reconcile app %s: port bindings mismatch, recreating container", appInst.InstanceID)
				_ = m.containerManager.StopContainer(ctx, runtime, containerID)
				_ = m.containerManager.RemoveContainer(ctx, runtime, containerID)
				m.serviceManager.RemoveApp(appInst.InstanceID)
				return m.recreateMissingContainer(ctx, state, appInst, def, layout, runtime)
			}
			log.Printf("WARN: reconcile app %s: publish reconcile failed: %v", appInst.InstanceID, err)
		}
	}
	return nil
}

func (m *AppManager) recreateMissingContainer(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance, def *api.AppDefinition, layout appVolumeLayout, runtime container.PodmanRuntime) error {
	if m.serviceManager == nil {
		return fmt.Errorf("app manager: service manager not configured")
	}

	// Check if this is a workspace app that needs --rootfs mode
	mode := piccoloModeFromExtensions(def.Extensions)
	var mergedPath string
	var workspaceMeta *workspacedisk.WorkspaceMeta
	if mode == ModeWorkspace {
		// Mount workspace disk overlay (idempotent)
		var err error
		mergedPath, err = m.ensureWorkspaceDiskMounted(ctx, appInst.InstanceID, layout)
		if err != nil {
			return fmt.Errorf("failed to mount workspace disk: %w", err)
		}

		// Get metadata for image config
		workspaceMeta, err = m.getWorkspaceDiskMeta(ctx, appInst.InstanceID, layout)
		if err != nil {
			return fmt.Errorf("failed to get workspace disk metadata: %w", err)
		}
		log.Printf("INFO: recreate %s: using workspace disk (base=%s)", appInst.InstanceID, workspaceMeta.BaseImageRef)
	}

	for attempt := 0; attempt < maxInstallPortRetries; attempt++ {
		endpoints, err := m.serviceManager.AllocateForApp(appInst.InstanceID, def.Listeners)
		if err != nil {
			return fmt.Errorf("allocate service ports: %w", err)
		}
		spec, err := m.appDefToContainerSpec(def, endpoints, layout, appInst.InstanceID)
		if err != nil {
			m.serviceManager.RemoveApp(appInst.InstanceID)
			return err
		}

		// For workspace mode, configure --rootfs and apply image config
		if mode == ModeWorkspace && mergedPath != "" && workspaceMeta != nil {
			spec.Rootfs = mergedPath
			spec.Image = ""

			// Apply image config (env, workdir, user) since Podman doesn't do it in --rootfs mode.
			spec.Environment = mergeEnvMaps(parseEnvSlice(workspaceMeta.ImageConfig.Env), spec.Environment)
			spec.WorkingDir = workspaceMeta.ImageConfig.WorkingDir
			spec.User = workspaceMeta.ImageConfig.User

			// Use boot.sh entrypoint with original command
			originalCmd := workspaceMeta.ImageConfig.BuildOriginalCommand()
			spec.Entrypoint = []string{"/bin/sh", "/piccolo/boot.sh"}
			spec.Command = originalCmd
		}

		cid, err := m.containerManager.CreateContainer(ctx, runtime, spec)
		if err == nil {
			if err := m.containerManager.StartContainer(ctx, runtime, cid); err != nil {
				_ = m.containerManager.RemoveContainer(ctx, runtime, cid)
				m.serviceManager.RemoveApp(appInst.InstanceID)
				return fmt.Errorf("failed to start container: %w", err)
			}
			appInst.ContainerID = cid
			_ = state.UpdateAppRuntime(appInst.InstanceID, "running", cid)
			m.serviceManager.SetAppContainerID(appInst.InstanceID, cid)
			return nil
		}

		var nameErr *container.NameInUseError
		if errors.As(err, &nameErr) {
			log.Printf("INFO: recreate app %s: adopted existing container %s", appInst.InstanceID, nameErr.ID)
			// Discard speculative port allocation; ensureServicesForRunningApp will restore actual ports.
			m.serviceManager.RemoveApp(appInst.InstanceID)
			appInst.ContainerID = nameErr.ID
			_ = state.UpdateAppRuntime(appInst.InstanceID, appInst.Status, nameErr.ID)
			return nil
		}

		var portErr *container.PortInUseError
		if errors.As(err, &portErr) {
			log.Printf("WARN: recreate app %s: host port conflict port=%d attempt=%d", appInst.InstanceID, portErr.Port, attempt)
			if portErr.Port > 0 {
				_ = m.serviceManager.ReserveHostPort(portErr.Port)
			} else {
				for _, ep := range endpoints {
					_ = m.serviceManager.ReserveHostPort(ep.HostBind)
				}
			}
			m.serviceManager.RemoveApp(appInst.InstanceID)
			continue
		}

		m.serviceManager.RemoveApp(appInst.InstanceID)
		return err
	}

	return fmt.Errorf("failed to recreate %s: exhausted host-port retries", appInst.InstanceID)
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
		m.serviceManager.SetAppContainerID(instanceID, containerID)
		return nil
	}

	if _, err := m.serviceManager.RestoreFromPodman(instanceID, def.Listeners, ports); err != nil {
		return err
	}
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

	expected := make(map[int]services.ServiceEndpoint, len(endpoints)) // guest -> endpoint
	for _, ep := range endpoints {
		expected[ep.GuestPort] = ep
	}

	// Check if any port reconciliation is needed.
	for guest, ep := range expected {
		host, ok := observed[guest]
		if !ok || host != ep.HostBind {
			// Podman does not support dynamic port binding updates on running containers.
			// Return error to trigger container recreation.
			return container.ErrPortReconciliationRequired
		}
	}
	for guest := range observed {
		if _, ok := expected[guest]; !ok {
			// Extra port exists that shouldn't - needs recreation.
			return container.ErrPortReconciliationRequired
		}
	}

	return nil
}

// Install installs a new application instance from its definition.
// The displayName parameter is an optional user-friendly name for the instance.
func (m *AppManager) Install(ctx context.Context, appDef *api.AppDefinition, displayName string) (*AppInstance, error) {
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()
	return m.installLocked(ctx, appDef, displayName)
}

func (m *AppManager) installLocked(ctx context.Context, appDef *api.AppDefinition, displayName string) (inst *AppInstance, err error) {
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

	state, err := m.ensureStateManager()
	if err != nil {
		return nil, err
	}

	// Generate unique instance ID
	existingIDs := state.ListInstanceIDs()
	instanceID, err = GenerateInstanceID(appDef.Name, existingIDs)
	if err != nil {
		return nil, fmt.Errorf("failed to generate instance ID: %w", err)
	}

	// Validate generated instance ID
	if err := ValidateInstanceID(instanceID); err != nil {
		return nil, fmt.Errorf("invalid generated instance ID: %w", err)
	}

	m.emitProgress(ctx, taskTypeInstallApp, instanceID, taskPhaseAllocatingPorts, 10, "Allocating ports", false, nil)
	inst, err = m.installWithRetries(ctx, state, appDef, instanceID, displayName, 0)
	return inst, err
}

func (m *AppManager) installWithRetries(ctx context.Context, state *FilesystemStateManager, appDef *api.AppDefinition, instanceID, displayName string, attempt int) (*AppInstance, error) {
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
	runtime, err := m.podmanRuntimeForApp(instanceID, layout)
	if err != nil {
		return nil, err
	}

	// Allocate services and convert to container spec using instanceID
	endpoints, err := m.serviceManager.AllocateForApp(instanceID, appDef.Listeners)
	if err != nil {
		return nil, fmt.Errorf("failed to allocate service ports: %w", err)
	}
	cleanupServices := true
	defer func() {
		if cleanupServices {
			m.serviceManager.RemoveApp(instanceID)
		}
	}()

	// Multi-container service-mode apps (compose-style) are installed as a group.
	if appDef.Services != nil {
		m.emitProgress(ctx, taskTypeInstallApp, instanceID, taskPhaseCreatingContainer, 60, "Creating containers", false, nil)
		app, err := m.installMultiContainer(ctx, appDef, instanceID, displayName, layout, runtime, endpoints)
		if err != nil {
			var portErr *container.PortInUseError
			if errors.As(err, &portErr) {
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
				return m.installWithRetries(ctx, state, appDef, instanceID, displayName, attempt+1)
			}
			return nil, err
		}

		m.emitProgress(ctx, taskTypeInstallApp, instanceID, taskPhaseRegisteringServices, 90, "Finalizing installation", false, nil)
		if err := state.StoreApp(app, appDef); err != nil {
			// Cleanup all containers if storage fails
			if app.NetworkAnchorID != "" {
				_ = m.containerManager.StopContainer(ctx, runtime, app.NetworkAnchorID)
				_ = m.containerManager.RemoveContainer(ctx, runtime, app.NetworkAnchorID)
			}
			for _, cid := range app.Containers {
				_ = m.containerManager.StopContainer(ctx, runtime, cid)
				_ = m.containerManager.RemoveContainer(ctx, runtime, cid)
			}
			m.serviceManager.RemoveApp(instanceID)
			cleanupServices = false
			return nil, fmt.Errorf("failed to store app: %w", err)
		}

		cleanupServices = false
		return app, nil
	}

	containerSpec, err := m.appDefToContainerSpec(appDef, endpoints, layout, instanceID)
	if err != nil {
		return nil, fmt.Errorf("failed to create container spec: %w", err)
	}

	mode := piccoloModeFromExtensions(appDef.Extensions)

	// Workspace mode: use container-independent persistence via workspace disk.
	// The workspace disk combines the base image (lowerdir) with a persistent
	// writable layer (upperdir) using fuse-overlayfs, eliminating the need for
	// podman commit snapshots.
	if mode == ModeWorkspace {
		// Check if workspace disk is already initialized (reinstall case)
		diskInitialized := m.isWorkspaceDiskInitialized(ctx, instanceID, layout)

		if !diskInitialized {
			// New install: pull base image and initialize workspace disk
			m.emitProgress(ctx, taskTypeInstallApp, instanceID, taskPhasePullingImage, 30, fmt.Sprintf("Pulling image %s", appDef.Image), false, nil)
			if err := m.containerManager.PullImage(ctx, runtime, appDef.Image); err != nil {
				log.Printf("WARN: install %s: image pull failed: %v", instanceID, err)
				m.emitProgress(ctx, taskTypeInstallApp, instanceID, taskPhasePullingImage, 30, fmt.Sprintf("Image pull failed (continuing): %v", err), false, nil)
			}

			// Get image config for workspace disk metadata
			imgConfig, err := m.containerManager.InspectImage(ctx, runtime, appDef.Image)
			if err != nil {
				return nil, fmt.Errorf("install %s: failed to inspect image %s: %w", instanceID, appDef.Image, err)
			}

			// Initialize and mount workspace disk
			m.emitProgress(ctx, taskTypeInstallApp, instanceID, taskPhaseInitializingDisk, 40, "Initializing workspace disk", false, nil)

			// Use the canonical digest from image inspect to ensure the same base image
			// is used across reinstalls and failovers (tags are mutable).
			// We prefer RepoDigests to ensure we have the registry context (repo@digest)
			// which is required for pulling on other nodes.
			baseImageDigest := ""
			if len(imgConfig.RepoDigests) > 0 {
				baseImageDigest = imgConfig.RepoDigests[0]
			} else {
				baseImageDigest = imgConfig.Digest
			}

			if baseImageDigest == "" {
				return nil, fmt.Errorf("install %s: image digest not available for %s", instanceID, appDef.Image)
			}
			mergedPath, err := m.initWorkspaceDisk(ctx, instanceID, layout, runtime, imgConfig, baseImageDigest, appDef.Image)
			if err != nil {
				return nil, fmt.Errorf("failed to initialize workspace disk: %w", err)
			}

			// Configure container to use --rootfs mode with the merged overlay
			containerSpec.Rootfs = mergedPath
			containerSpec.Image = "" // Clear image since we're using rootfs

			// Apply image config (env, workdir, user) since Podman doesn't do it in --rootfs mode.
			// Base image env is merged with manifest env (manifest takes precedence).
			containerSpec.Environment = mergeEnvMaps(parseEnvSlice(imgConfig.Env), containerSpec.Environment)
			containerSpec.WorkingDir = imgConfig.WorkingDir
			containerSpec.User = imgConfig.User

			// Wrap entrypoint with boot.sh
			originalCmd := buildOriginalCommand(imgConfig)
			containerSpec.Entrypoint = []string{"/bin/sh", "/piccolo/boot.sh"}
			containerSpec.Command = originalCmd
		} else {
			// Reinstall: workspace disk exists, just mount it
			m.emitProgress(ctx, taskTypeInstallApp, instanceID, taskPhaseMountingWorkspace, 30, "Mounting workspace disk", false, nil)

			mergedPath, err := m.ensureWorkspaceDiskMounted(ctx, instanceID, layout)
			if err != nil {
				return nil, fmt.Errorf("failed to mount workspace disk: %w", err)
			}

			// Get metadata for entrypoint config
			meta, err := m.getWorkspaceDiskMeta(ctx, instanceID, layout)
			if err != nil {
				return nil, fmt.Errorf("failed to get workspace disk metadata: %w", err)
			}

			// Configure container to use --rootfs mode
			containerSpec.Rootfs = mergedPath
			containerSpec.Image = ""

			// Apply image config (env, workdir, user) since Podman doesn't do it in --rootfs mode.
			// Base image env is merged with manifest env (manifest takes precedence).
			containerSpec.Environment = mergeEnvMaps(parseEnvSlice(meta.ImageConfig.Env), containerSpec.Environment)
			containerSpec.WorkingDir = meta.ImageConfig.WorkingDir
			containerSpec.User = meta.ImageConfig.User

			// Use entrypoint from saved metadata
			originalCmd := meta.ImageConfig.BuildOriginalCommand()
			containerSpec.Entrypoint = []string{"/bin/sh", "/piccolo/boot.sh"}
			containerSpec.Command = originalCmd

			log.Printf("INFO: install %s: using existing workspace disk (base=%s)", instanceID, meta.BaseImageRef)
		}
	} else {
		// Service mode: pull image normally
		m.emitProgress(ctx, taskTypeInstallApp, instanceID, taskPhasePullingImage, 30, fmt.Sprintf("Pulling image %s", appDef.Image), false, nil)
		if err := m.containerManager.PullImage(ctx, runtime, appDef.Image); err != nil {
			log.Printf("WARN: install %s: image pull failed: %v", instanceID, err)
			m.emitProgress(ctx, taskTypeInstallApp, instanceID, taskPhasePullingImage, 30, fmt.Sprintf("Image pull failed (continuing): %v", err), false, nil)
		}
	}

	// Create container with zombie cleanup
	var containerID string
	m.emitProgress(ctx, taskTypeInstallApp, instanceID, taskPhaseCreatingContainer, 60, "Creating container", false, nil)
	for i := 0; i < 2; i++ {
		containerID, err = m.containerManager.CreateContainer(ctx, runtime, containerSpec)
		if err == nil {
			break
		}

		// If PortInUse, don't local retry - let the outer recursion handle it
		var portErr *container.PortInUseError
		if errors.As(err, &portErr) {
			break
		}

		// Check for cleanup opportunities (NameInUse or Zombie)
		zombieID := ""
		var nameErr *container.NameInUseError
		if errors.As(err, &nameErr) {
			zombieID = nameErr.ID
		} else if id, resolveErr := m.containerManager.ResolveContainerIDByName(ctx, runtime, instanceID); resolveErr == nil {
			zombieID = id
		}

		if zombieID != "" {
			log.Printf("INFO: install %s: removing zombie container %s", instanceID, zombieID)
			_ = m.containerManager.RemoveContainer(ctx, runtime, zombieID)
			// Continue loop to retry creation
			continue
		}

		// If no zombie to clean, hard failure
		break
	}

	if err != nil {
		var portErr *container.PortInUseError
		if errors.As(err, &portErr) {
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
			return m.installWithRetries(ctx, state, appDef, instanceID, displayName, attempt+1)
		}

		return nil, fmt.Errorf("failed to create container: %w", err)
	}

	// Start container immediately
	m.emitProgress(ctx, taskTypeInstallApp, instanceID, taskPhaseStarting, 80, "Starting container", false, nil)
	if err := m.containerManager.StartContainer(ctx, runtime, containerID); err != nil {
		// Atomic install: if start fails, cleanup and fail.
		// We do NOT persist the app state, so the user can retry.
		_ = m.containerManager.RemoveContainer(ctx, runtime, containerID)
		// Defer will cleanup services
		return nil, fmt.Errorf("failed to start container: %w", err)
	}

	// Record container ID for watcher reconciliation
	if m.serviceManager != nil {
		m.serviceManager.SetAppContainerID(instanceID, containerID)
	}

	// Create app instance with embedded definition
	now := time.Now()
	app := &AppInstance{
		InstanceID:  instanceID,
		DisplayName: displayName,
		Status:      "running",
		ContainerID: containerID,
		CreatedAt:   now,
		UpdatedAt:   now,
		Definition:  appDef,
	}

	// Store app to filesystem
	m.emitProgress(ctx, taskTypeInstallApp, instanceID, taskPhaseRegisteringServices, 90, "Finalizing installation", false, nil)
	if err := state.StoreApp(app, appDef); err != nil {
		// Cleanup container if storage fails
		_ = m.containerManager.StopContainer(ctx, runtime, containerID)
		_ = m.containerManager.RemoveContainer(ctx, runtime, containerID)
		m.serviceManager.RemoveApp(instanceID)
		cleanupServices = false
		return nil, fmt.Errorf("failed to store app: %w", err)
	}

	cleanupServices = false

	return app, nil
}

// Upsert installs a new application instance.
// Deprecated: With multi-instance support, use Install() directly.
// This method now always creates a new instance (no update behavior).
func (m *AppManager) Upsert(ctx context.Context, appDef *api.AppDefinition) (*AppInstance, error) {
	return m.Install(ctx, appDef, "")
}

// List returns all installed applications
func (m *AppManager) List(ctx context.Context) ([]*AppInstance, error) {
	state, err := m.ensureStateManager()
	if err != nil {
		return nil, err
	}
	return state.ListApps(), nil
}

// Get returns a specific application instance by instanceID.
func (m *AppManager) Get(ctx context.Context, instanceID string) (*AppInstance, error) {
	state, err := m.ensureStateManager()
	if err != nil {
		return nil, err
	}
	app, exists := state.GetApp(instanceID)
	if !exists {
		return nil, fmt.Errorf("app instance not found: %s", instanceID)
	}

	return app, nil
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
	runtime, err := m.podmanRuntimeForApp(instanceID, layout)
	if err != nil {
		return err
	}

	// For workspace mode apps, ensure the overlay is mounted before starting.
	// After a host restart or daemon crash, the fuse-overlayfs mount will be gone,
	// so we must remount it before the --rootfs container can start.
	def, defErr := state.GetAppDefinition(instanceID)
	if defErr == nil {
		if def.Services != nil {
			// Multi-container service-mode app: start network anchor + services in order.
			m.emitProgress(ctx, taskTypeStartApp, instanceID, taskPhaseStarting, 60, "Starting containers", false, nil)
			return m.startMultiContainer(ctx, state, app, def, layout, runtime)
		}
		mode := piccoloModeFromExtensions(def.Extensions)
		if mode == ModeWorkspace {
			m.emitProgress(ctx, taskTypeStartApp, instanceID, taskPhaseMountingWorkspace, 30, "Mounting workspace disk", false, nil)

			// Cleanup any stale mounts from previous crashes (RFC §5.6)
			m.cleanupStaleWorkspaceMounts(ctx, instanceID, layout)

			// Ensure workspace disk is mounted
			if _, err := m.ensureWorkspaceDiskMounted(ctx, instanceID, layout); err != nil {
				_ = state.UpdateAppStatus(instanceID, "error")
				return fmt.Errorf("failed to mount workspace disk: %w", err)
			}
		}
	}

	// Start the container
	m.emitProgress(ctx, taskTypeStartApp, instanceID, taskPhaseStarting, 60, "Starting container", false, nil)
	if err := m.containerManager.StartContainer(ctx, runtime, app.ContainerID); err != nil {
		// Update status to error
		_ = state.UpdateAppStatus(instanceID, "error")
		return fmt.Errorf("failed to start container: %w", err)
	}

	// Update status to running
	m.emitProgress(ctx, taskTypeStartApp, instanceID, taskPhaseUpdatingServices, 80, "Updating services", false, nil)
	if err := state.UpdateAppStatus(instanceID, "running"); err != nil {
		return fmt.Errorf("failed to update app status: %w", err)
	}

	// Rehydrate service proxies if they were removed while the app was stopped
	if _, err := m.serviceManager.GetByApp(instanceID); err != nil {
		def, defErr := state.GetAppDefinition(instanceID)
		if defErr != nil {
			log.Printf("WARN: start app %s: failed to load app definition: %v", instanceID, defErr)
		} else {
			ports, portErr := m.containerManager.InspectPublishedPorts(ctx, runtime, app.ContainerID)
			if portErr != nil {
				log.Printf("WARN: start app %s: inspect ports failed: %v", instanceID, portErr)
			} else if len(ports) == 0 {
				log.Printf("WARN: start app %s: no published ports found during restore", instanceID)
			} else {
				if _, restoreErr := m.serviceManager.RestoreFromPodman(instanceID, def.Listeners, ports); restoreErr != nil {
					log.Printf("WARN: start app %s: failed to restore services: %v", instanceID, restoreErr)
				} else {
					m.serviceManager.SetAppContainerID(instanceID, app.ContainerID)
				}
			}
		}
	}

	return nil
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
	runtime, err := m.podmanRuntimeForApp(instanceID, layout)
	if err != nil {
		return err
	}

	def, defErr := state.GetAppDefinition(instanceID)
	if defErr == nil && def.Services != nil {
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
			m.serviceManager.RemoveApp(instanceID)
		}
		return nil
	}

	if err := m.containerManager.StopContainer(ctx, runtime, app.ContainerID); err != nil {
		var notFound *container.ContainerNotFoundError
		if !errors.As(err, &notFound) {
			return err
		}
	}
	if m.serviceManager != nil {
		m.serviceManager.RemoveApp(instanceID)
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
	runtime, err := m.podmanRuntimeForApp(instanceID, layout)
	if err != nil {
		return err
	}

	def, defErr := state.GetAppDefinition(instanceID)
	if defErr == nil && def.Services != nil {
		m.emitProgress(ctx, taskTypeStopApp, instanceID, taskPhaseStopping, 40, "Stopping containers", false, nil)
		return m.stopMultiContainer(ctx, state, app, def, runtime)
	}

	if err := m.containerManager.StopContainer(ctx, runtime, app.ContainerID); err != nil {
		_ = state.UpdateAppStatus(instanceID, "error")
		return fmt.Errorf("failed to stop container: %w", err)
	}

	// For workspace mode apps, unmount the overlay on clean stop (RFC §5.6).
	// This is good practice but not strictly required since we remount on start.
	if defErr == nil {
		mode := piccoloModeFromExtensions(def.Extensions)
		if mode == ModeWorkspace {
			m.emitProgress(ctx, taskTypeStopApp, instanceID, taskPhaseUnmountingWorkspace, 60, "Unmounting workspace disk", false, nil)
			if err := m.unmountWorkspaceDisk(ctx, instanceID); err != nil {
				// Log but don't fail - the data is safe, mount will be cleaned up on next start
				log.Printf("WARN: stop %s: failed to unmount workspace disk: %v", instanceID, err)
			}
		}
	}

	m.emitProgress(ctx, taskTypeStopApp, instanceID, taskPhaseUpdatingServices, 80, "Updating services", false, nil)
	if err := state.UpdateAppStatus(instanceID, "stopped"); err != nil {
		return fmt.Errorf("failed to update app status: %w", err)
	}

	if m.serviceManager != nil {
		m.serviceManager.RemoveApp(instanceID)
	}

	return nil
}

// Uninstall removes an application instance completely by instanceID.
func (m *AppManager) Uninstall(ctx context.Context, instanceID string) error {
	if err := m.ensureUnlocked(); err != nil {
		return err
	}
	if err := m.ensureKernelLeader(); err != nil {
		return err
	}
	return m.UninstallWithOptions(ctx, instanceID, false)
}

// UninstallWithOptions removes an application instance; when purge is true, also deletes app data directories.
func (m *AppManager) UninstallWithOptions(ctx context.Context, instanceID string, purge bool) error {
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()
	return m.uninstallLocked(ctx, instanceID, purge)
}

func (m *AppManager) uninstallLocked(ctx context.Context, instanceID string, purge bool) (err error) {
	m.emitProgress(ctx, taskTypeUninstallApp, instanceID, taskPhaseStopping, 0, "Stopping app", false, nil)
	defer func() {
		if err != nil {
			m.emitProgress(ctx, taskTypeUninstallApp, instanceID, taskPhaseComplete, 100, "Uninstall failed", true, err)
			return
		}
		m.emitProgress(ctx, taskTypeUninstallApp, instanceID, taskPhaseComplete, 100, "Uninstalled", true, nil)
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
	runtime, err := m.podmanRuntimeForApp(instanceID, layout)
	if err != nil {
		return err
	}

	def, defErr := state.GetAppDefinition(instanceID)
	if defErr == nil && def.Services != nil {
		primary := primaryServiceFor(def, app)
		order, _ := serviceStartOrder(def.Services)

		// Stop containers best-effort.
		for i := len(order) - 1; i >= 0; i-- {
			svcName := order[i]
			cid := strings.TrimSpace(app.Containers[svcName])
			if cid == "" {
				name := containerNameForService(instanceID, svcName, primary)
				if id, err := m.containerManager.ResolveContainerIDByName(ctx, runtime, name); err == nil {
					cid = id
				}
			}
			if cid != "" {
				_ = m.containerManager.StopContainer(ctx, runtime, cid)
			}
		}
		anchorID := strings.TrimSpace(app.NetworkAnchorID)
		if anchorID == "" {
			if id, err := m.containerManager.ResolveContainerIDByName(ctx, runtime, networkAnchorContainerName(instanceID)); err == nil {
				anchorID = id
			}
		}
		if anchorID != "" {
			_ = m.containerManager.StopContainer(ctx, runtime, anchorID)
		}

		// Remove containers.
		m.emitProgress(ctx, taskTypeUninstallApp, instanceID, taskPhaseRemovingContainer, 40, "Removing containers", false, nil)
		for i := len(order) - 1; i >= 0; i-- {
			svcName := order[i]
			cid := strings.TrimSpace(app.Containers[svcName])
			if cid == "" {
				name := containerNameForService(instanceID, svcName, primary)
				if id, err := m.containerManager.ResolveContainerIDByName(ctx, runtime, name); err == nil {
					cid = id
				}
			}
			if cid != "" {
				_ = m.containerManager.RemoveContainer(ctx, runtime, cid)
			}
		}
		if anchorID != "" {
			if err := m.containerManager.RemoveContainer(ctx, runtime, anchorID); err != nil {
				return fmt.Errorf("failed to remove network anchor: %w", err)
			}
		}

		// Stop and remove service listeners for this app
		if m.serviceManager != nil {
			m.serviceManager.RemoveApp(instanceID)
		}

		// Optionally purge app data (destroy volume and podman runtime state)
		if purge {
			m.emitProgress(ctx, taskTypeUninstallApp, instanceID, taskPhaseCleaningVolumes, 80, "Purging app data", false, nil)
			// Reset podman storage to clean up any remaining containers
			if err := m.containerManager.ResetStorage(ctx, runtime); err != nil {
				log.Printf("WARN: podman storage reset for %s failed: %v", instanceID, err)
			}

			volID := appVolumeID(instanceID)
			if err := m.volumeManager.DestroyVolume(ctx, volID); err != nil {
				return fmt.Errorf("failed to purge app data: %w", err)
			}

			// Remove podman runRoot which lives outside the encrypted volume
			if err := os.RemoveAll(runtime.RunRoot); err != nil {
				log.Printf("WARN: failed to remove podman runRoot %s: %v", runtime.RunRoot, err)
			}
		}

		// Remove from filesystem and cache (state only)
		if err := state.RemoveApp(instanceID); err != nil {
			return fmt.Errorf("failed to remove app from storage: %w", err)
		}

		return nil
	}

	// Stop container first (ignore error if already stopped)
	_ = m.containerManager.StopContainer(ctx, runtime, app.ContainerID)

	// For workspace mode apps, unmount the workspace disk overlay.
	// With workspace disk, no snapshot is needed - data persists independently of the container.
	if defErr == nil {
		mode := piccoloModeFromExtensions(def.Extensions)
		if mode == ModeWorkspace {
			m.emitProgress(ctx, taskTypeUninstallApp, instanceID, taskPhaseUnmountingWorkspace, 20, "Unmounting workspace disk", false, nil)
			if err := m.unmountWorkspaceDisk(ctx, instanceID); err != nil {
				log.Printf("WARN: workspace %s: failed to unmount workspace disk: %v", instanceID, err)
			} else {
				log.Printf("INFO: workspace %s: unmounted workspace disk (data preserved)", instanceID)
			}
		}
	}

	// Remove container
	m.emitProgress(ctx, taskTypeUninstallApp, instanceID, taskPhaseRemovingContainer, 40, "Removing container", false, nil)
	if err := m.containerManager.RemoveContainer(ctx, runtime, app.ContainerID); err != nil {
		return fmt.Errorf("failed to remove container: %w", err)
	}

	// Stop and remove service listeners for this app
	if m.serviceManager != nil {
		m.serviceManager.RemoveApp(instanceID)
	}

	// Optionally purge app data (destroy volume and podman runtime state)
	if purge {
		m.emitProgress(ctx, taskTypeUninstallApp, instanceID, taskPhaseCleaningVolumes, 80, "Purging app data", false, nil)
		// Reset podman storage to clean up any remaining containers
		if err := m.containerManager.ResetStorage(ctx, runtime); err != nil {
			log.Printf("WARN: podman storage reset for %s failed: %v", instanceID, err)
		}

		volID := appVolumeID(instanceID)
		if err := m.volumeManager.DestroyVolume(ctx, volID); err != nil {
			return fmt.Errorf("failed to purge app data: %w", err)
		}

		// Remove podman runRoot which lives outside the encrypted volume
		if err := os.RemoveAll(runtime.RunRoot); err != nil {
			log.Printf("WARN: failed to remove podman runRoot %s: %v", runtime.RunRoot, err)
		}
	}

	// Remove from filesystem and cache (state only)
	if err := state.RemoveApp(instanceID); err != nil {
		return fmt.Errorf("failed to remove app from storage: %w", err)
	}

	return nil
}

// Enable enables an application instance (systemctl-style) by instanceID.
func (m *AppManager) Enable(ctx context.Context, instanceID string) error {
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
	if _, exists := state.GetApp(instanceID); !exists {
		return fmt.Errorf("app instance not found: %s", instanceID)
	}

	return state.EnableApp(instanceID)
}

// Disable disables an application instance (systemctl-style) by instanceID.
func (m *AppManager) Disable(ctx context.Context, instanceID string) error {
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
	if _, exists := state.GetApp(instanceID); !exists {
		return fmt.Errorf("app instance not found: %s", instanceID)
	}

	return state.DisableApp(instanceID)
}

// IsEnabled checks if an application instance is enabled by instanceID.
func (m *AppManager) IsEnabled(ctx context.Context, instanceID string) (bool, error) {
	state, err := m.ensureStateManager()
	if err != nil {
		return false, err
	}
	if _, exists := state.GetApp(instanceID); !exists {
		return false, fmt.Errorf("app instance not found: %s", instanceID)
	}

	return state.IsAppEnabled(instanceID), nil
}

// ListEnabled returns instanceIDs of all enabled app instances.
func (m *AppManager) ListEnabled(ctx context.Context) ([]string, error) {
	state, err := m.ensureStateManager()
	if err != nil {
		return nil, err
	}
	return state.ListEnabledApps()
}

// UpdateImage updates an app instance's container image tag and recreates the container preserving services.
func (m *AppManager) UpdateImage(ctx context.Context, instanceID string, tag *string) error {
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()
	return m.updateImageLocked(ctx, instanceID, tag)
}

func (m *AppManager) updateImageLocked(ctx context.Context, instanceID string, tag *string) error {
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
	runtime, err := m.podmanRuntimeForApp(instanceID, layout)
	if err != nil {
		return err
	}
	// Load current app definition
	curDef, err := state.GetAppDefinition(instanceID)
	if err != nil {
		return fmt.Errorf("failed to read current app.yaml: %w", err)
	}
	if curDef.Services != nil {
		return fmt.Errorf("cannot update image for multi-container apps; update per-service images in the manifest and reinstall")
	}

	// Workspace mode apps cannot have their image updated because the workspace disk
	// overlay is the persistence mechanism. Changing the base image would require
	// "rebasing" the overlay which is complex and out of scope (see RFC non-goals).
	// Users who want a new base image should uninstall and reinstall the workspace.
	mode := piccoloModeFromExtensions(curDef.Extensions)
	if mode == ModeWorkspace {
		return fmt.Errorf("cannot update image for workspace apps: workspace persistence is tied to the base image; uninstall and reinstall to use a different base image")
	}

	// Compute new image
	newImage := curDef.Image
	if tag != nil {
		// Replace tag portion if present, or append
		img := curDef.Image
		// Split on ':' but be careful with registry includes ':'
		// Strategy: if '@' digest present, ignore; else change last ':' segment after last '/'
		if i := strings.LastIndex(img, "/"); i >= 0 {
			repo := img[:i+1]
			rest := img[i+1:]
			if j := strings.LastIndex(rest, ":"); j >= 0 {
				newImage = repo + rest[:j] + ":" + *tag
			} else {
				newImage = repo + rest + ":" + *tag
			}
		} else {
			if j := strings.LastIndex(img, ":"); j >= 0 {
				newImage = img[:j] + ":" + *tag
			} else {
				newImage = img + ":" + *tag
			}
		}
	}
	// Prepare new def
	newDef := *curDef
	newDef.Image = newImage
	// Backup current YAML and validate new
	if err := ValidateAppDefinition(&newDef); err != nil {
		return fmt.Errorf("invalid new app definition: %w", err)
	}
	if err := state.BackupCurrentAppDefinition(instanceID); err != nil {
		return fmt.Errorf("backup app.yaml: %w", err)
	}
	// Pull image to app's storage (best effort)
	_ = m.containerManager.PullImage(ctx, runtime, newImage)
	// Preserve endpoints
	endpoints, _ := m.serviceManager.GetByApp(instanceID)
	// Stop and remove old container
	_ = m.containerManager.StopContainer(ctx, runtime, appInst.ContainerID)
	_ = m.containerManager.RemoveContainer(ctx, runtime, appInst.ContainerID)
	// Create new container with same endpoints
	spec, err := m.appDefToContainerSpec(&newDef, endpoints, layout, instanceID)
	if err != nil {
		return fmt.Errorf("build container spec: %w", err)
	}
	newCID, err := m.containerManager.CreateContainer(ctx, runtime, spec)
	if err != nil {
		return fmt.Errorf("create container: %w", err)
	}
	if m.serviceManager != nil {
		m.serviceManager.SetAppContainerID(instanceID, newCID)
	}
	startErr := m.containerManager.StartContainer(ctx, runtime, newCID)
	// Update instance with new definition and persist
	appInst.Definition = &newDef
	appInst.ContainerID = newCID
	if startErr != nil {
		appInst.Status = "error"
	} else {
		appInst.Status = "running"
	}
	appInst.UpdatedAt = time.Now()
	if err := state.StoreApp(appInst, nil); err != nil {
		_ = m.containerManager.StopContainer(ctx, runtime, newCID)
		_ = m.containerManager.RemoveContainer(ctx, runtime, newCID)
		return fmt.Errorf("store app: %w", err)
	}
	if startErr != nil {
		return fmt.Errorf("start container: %w", startErr)
	}
	return nil
}

// UpdateListeners updates an app instance's listeners and recreates the container if necessary.
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
	runtime, err := m.podmanRuntimeForApp(instanceID, layout)
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
		// With workspace disk, container recreation is always safe - no snapshot needed.
		// The workspace disk persists independently of the container, so we can simply
		// stop/remove/recreate the container wrapper without data loss.
		m.emitProgress(ctx, taskTypeUpdateListeners, instanceID, taskPhaseRecreatingContainer, 50, "Recreating container", false, nil)

		// Stop and remove old container
		_ = m.containerManager.StopContainer(ctx, runtime, appInst.ContainerID)
		_ = m.containerManager.RemoveContainer(ctx, runtime, appInst.ContainerID)

		// Ensure workspace disk is mounted and get the merged path
		mergedPath, err := m.ensureWorkspaceDiskMounted(ctx, instanceID, layout)
		if err != nil {
			return nil, fmt.Errorf("failed to ensure workspace disk mounted: %w", err)
		}

		// Get metadata for entrypoint config
		meta, err := m.getWorkspaceDiskMeta(ctx, instanceID, layout)
		if err != nil {
			return nil, fmt.Errorf("failed to get workspace disk metadata: %w", err)
		}

		// Create new container with updated endpoints and --rootfs mode
		var newCID string
		spec, err := m.appDefToContainerSpec(&newDef, result.Endpoints, layout, instanceID)
		if err == nil {
			// Use --rootfs mode with workspace disk
			spec.Rootfs = mergedPath
			spec.Image = ""

			// Apply image config (env, workdir, user) since Podman doesn't do it in --rootfs mode.
			spec.Environment = mergeEnvMaps(parseEnvSlice(meta.ImageConfig.Env), spec.Environment)
			spec.WorkingDir = meta.ImageConfig.WorkingDir
			spec.User = meta.ImageConfig.User

			// Use entrypoint from saved metadata
			originalCmd := meta.ImageConfig.BuildOriginalCommand()
			spec.Entrypoint = []string{"/bin/sh", "/piccolo/boot.sh"}
			spec.Command = originalCmd

			newCID, err = m.containerManager.CreateContainer(ctx, runtime, spec)
		}

		if err != nil {
			// Attempt rollback - with workspace disk this is simpler since data is safe
			log.Printf("WARN: update listeners %s: creation failed: %v. Rolling back...", instanceID, err)

			// 1. Revert ports
			rbResult, _, rbErr := m.serviceManager.Reconcile(instanceID, curDef.Listeners)
			if rbErr != nil {
				log.Printf("ERROR: update listeners %s: port rollback failed: %v", instanceID, rbErr)
				appInst.Status = "error"
				appInst.ContainerID = ""
				_ = state.StoreApp(appInst, curDef)
				return nil, fmt.Errorf("update failed: %w; rollback failed (ports): %v", err, rbErr)
			}

			// 2. Rebuild old spec with --rootfs mode
			rbSpec, rbErr := m.appDefToContainerSpec(curDef, rbResult.Endpoints, layout, instanceID)
			if rbErr != nil {
				log.Printf("ERROR: update listeners %s: spec rollback failed: %v", instanceID, rbErr)
				appInst.Status = "error"
				appInst.ContainerID = ""
				_ = state.StoreApp(appInst, curDef)
				return nil, fmt.Errorf("update failed: %w; rollback failed (spec): %v", err, rbErr)
			}

			// Use --rootfs mode for rollback too
			rbSpec.Rootfs = mergedPath
			rbSpec.Image = ""
			rbSpec.Environment = mergeEnvMaps(parseEnvSlice(meta.ImageConfig.Env), rbSpec.Environment)
			rbSpec.WorkingDir = meta.ImageConfig.WorkingDir
			rbSpec.User = meta.ImageConfig.User
			rbSpec.Entrypoint = []string{"/bin/sh", "/piccolo/boot.sh"}
			rbSpec.Command = meta.ImageConfig.BuildOriginalCommand()

			// 3. Create old container
			rbCID, rbErr := m.containerManager.CreateContainer(ctx, runtime, rbSpec)
			if rbErr != nil {
				log.Printf("ERROR: update listeners %s: container rollback failed: %v", instanceID, rbErr)
				appInst.Status = "error"
				appInst.ContainerID = ""
				_ = state.StoreApp(appInst, curDef)
				return nil, fmt.Errorf("update failed: %w; rollback failed (create): %v", err, rbErr)
			}

			// 4. Start old container
			if rbErr := m.containerManager.StartContainer(ctx, runtime, rbCID); rbErr != nil {
				log.Printf("ERROR: update listeners %s: start rollback failed: %v", instanceID, rbErr)
				appInst.Status = "error"
				appInst.ContainerID = rbCID // It exists but failed to start
				_ = state.StoreApp(appInst, curDef)
				return nil, fmt.Errorf("update failed: %w; rollback failed (start): %v", err, rbErr)
			}

			// Rollback successful
			log.Printf("INFO: update listeners %s: rollback successful", instanceID)
			if m.serviceManager != nil {
				m.serviceManager.SetAppContainerID(instanceID, rbCID)
			}
			appInst.ContainerID = rbCID
			appInst.Status = "running"
			// Save the restored state to ensure persistence of new CID
			if saveErr := state.StoreApp(appInst, curDef); saveErr != nil {
				log.Printf("WARN: update listeners %s: failed to save rollback state: %v", instanceID, saveErr)
			}

			return nil, fmt.Errorf("update failed: %w (rolled back to previous state)", err)
		}

		if m.serviceManager != nil {
			m.serviceManager.SetAppContainerID(instanceID, newCID)
		}

		// Update instance
		appInst.ContainerID = newCID

		// Start container automatically
		m.emitProgress(ctx, taskTypeUpdateListeners, instanceID, taskPhaseStarting, 80, "Starting container", false, nil)
		if err := m.containerManager.StartContainer(ctx, runtime, newCID); err != nil {
			appInst.Status = "error"
			log.Printf("WARN: update listeners %s: failed to start new container: %v", instanceID, err)
		} else {
			appInst.Status = "running"
		}
	}

	m.emitProgress(ctx, taskTypeUpdateListeners, instanceID, taskPhaseFinalizing, 90, "Saving configuration", false, nil)
	appInst.UpdatedAt = time.Now()
	if err := state.StoreApp(appInst, &newDef); err != nil {
		return nil, fmt.Errorf("store app: %w", err)
	}

	return &newDef, nil
}

// Revert reverts an app instance to the previous app.yaml (if available) and recreates container.
func (m *AppManager) Revert(ctx context.Context, instanceID string) error {
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()
	return m.revertLocked(ctx, instanceID)
}

func (m *AppManager) revertLocked(ctx context.Context, instanceID string) error {
	if err := m.ensureUnlocked(); err != nil {
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
	runtime, err := m.podmanRuntimeForApp(instanceID, layout)
	if err != nil {
		return err
	}
	// Read previous def
	prevDef, err := state.GetPreviousAppDefinition(instanceID)
	if err != nil {
		return fmt.Errorf("no previous version to revert to: %w", err)
	}
	if prevDef.Services != nil || (appInst.Definition != nil && appInst.Definition.Services != nil) {
		return fmt.Errorf("revert is not supported for multi-container apps")
	}
	// Backup current before writing previous
	if err := state.BackupCurrentAppDefinition(instanceID); err != nil {
		return fmt.Errorf("backup current: %w", err)
	}
	// Preserve endpoints
	endpoints, _ := m.serviceManager.GetByApp(instanceID)
	// Stop and remove current container
	_ = m.containerManager.StopContainer(ctx, runtime, appInst.ContainerID)
	_ = m.containerManager.RemoveContainer(ctx, runtime, appInst.ContainerID)
	// Pull to app's storage (best-effort)
	if prevDef.Image != "" {
		_ = m.containerManager.PullImage(ctx, runtime, prevDef.Image)
	}
	// Create new container from prev
	spec, err := m.appDefToContainerSpec(prevDef, endpoints, layout, instanceID)
	if err != nil {
		return fmt.Errorf("build container spec: %w", err)
	}
	newCID, err := m.containerManager.CreateContainer(ctx, runtime, spec)
	if err != nil {
		return fmt.Errorf("create container: %w", err)
	}
	if m.serviceManager != nil {
		m.serviceManager.SetAppContainerID(instanceID, newCID)
	}
	startErr := m.containerManager.StartContainer(ctx, runtime, newCID)
	// Update instance with previous definition and persist
	appInst.Definition = prevDef
	appInst.ContainerID = newCID
	if startErr != nil {
		appInst.Status = "error"
	} else {
		appInst.Status = "running"
	}
	appInst.UpdatedAt = time.Now()
	if err := state.StoreApp(appInst, nil); err != nil {
		_ = m.containerManager.StopContainer(ctx, runtime, newCID)
		_ = m.containerManager.RemoveContainer(ctx, runtime, newCID)
		return fmt.Errorf("store app: %w", err)
	}
	if startErr != nil {
		return fmt.Errorf("start container: %w", startErr)
	}
	return nil
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
// If service is empty, defaults to the primary service (or the single container for legacy apps).
func (m *AppManager) LogsForService(ctx context.Context, instanceID, service string, lines int) ([]string, error) {
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
	runtime, err := m.podmanRuntimeForApp(instanceID, layout)
	if err != nil {
		return nil, err
	}

	def := appInst.Definition
	if def != nil && def.Services != nil {
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
				if target == primary {
					appInst.ContainerID = id
				}
				_ = state.StoreApp(appInst, nil)
			}
		}
		if cid == "" {
			return nil, fmt.Errorf("container not found for service '%s'", target)
		}
		return m.containerManager.Logs(ctx, runtime, cid, lines)
	}

	return m.containerManager.Logs(ctx, runtime, appInst.ContainerID, lines)
}

// LogsStreamForService returns a follow-stream of container logs for a specific service container in an app instance.
// If service is empty, defaults to the primary service (or the single container for legacy apps).
func (m *AppManager) LogsStreamForService(ctx context.Context, instanceID, service string, lines int, timestamps bool) (io.ReadCloser, error) {
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
	runtime, err := m.podmanRuntimeForApp(instanceID, layout)
	if err != nil {
		return nil, err
	}

	def := appInst.Definition
	if def != nil && def.Services != nil {
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
				if target == primary {
					appInst.ContainerID = id
				}
				_ = state.StoreApp(appInst, nil)
			}
		}
		if cid == "" {
			return nil, fmt.Errorf("container not found for service '%s'", target)
		}
		return m.containerManager.LogsStream(ctx, runtime, cid, lines, timestamps)
	}

	return m.containerManager.LogsStream(ctx, runtime, appInst.ContainerID, lines, timestamps)
}

// appDefToContainerSpec converts an AppDefinition to a ContainerCreateSpec.
// storage volumes are mapped into the per-app encrypted volume at <mount>/data/<volume-name>.
// The instanceID parameter is the unique instance identifier used for container naming.
func (m *AppManager) appDefToContainerSpec(appDef *api.AppDefinition, endpoints []services.ServiceEndpoint, layout appVolumeLayout, instanceID string) (container.ContainerCreateSpec, error) {
	spec := container.ContainerCreateSpec{
		Name:        instanceID,
		Image:       appDef.Image,
		Environment: appDef.Environment,
		Labels:      piccoloLabels(instanceID, defaultPrimaryServiceName, "service"),
	}

	// Convert listeners to port mappings using allocated endpoints
	for _, ep := range endpoints {
		spec.Ports = append(spec.Ports, container.PortMapping{
			Host:      ep.HostBind,
			Container: ep.GuestPort,
		})
	}

	// Convert resources if present
	if appDef.Resources != nil && appDef.Resources.Limits != nil {
		spec.Resources = container.ResourceLimits{
			Memory: appDef.Resources.Limits.Memory,
			CPU:    fmt.Sprintf("%.1f", appDef.Resources.Limits.CPU),
		}
	}

	// Set network mode based on permissions
	if appDef.Permissions != nil && appDef.Permissions.Network != nil {
		if appDef.Permissions.Network.Internet == "deny" {
			spec.NetworkMode = "none"
		}
	}

	// Set restart policy for system apps
	if appDef.Type == "system" {
		spec.RestartPolicy = "always"
	}

	// Storage mounts:
	// - storage.persistent -> bind mounts inside <app-volume>/data/<volume-name>
	// - storage.temporary  -> tmpfs mounts (ephemeral)
	if err := m.applyServiceStorageAndTmpfs(&spec, appDef.Storage, layout, appDef.Extensions); err != nil {
		return spec, err
	}

	// Workspace mode: enable init and mount boot.sh wrapper
	mode := piccoloModeFromExtensions(appDef.Extensions)
	if mode == ModeWorkspace {
		// Use --init for proper PID 1 signal handling and zombie reaping
		spec.UseInit = true

		// Ensure workspace assets exist on host filesystem
		if err := EnsureWorkspaceAssets(); err != nil {
			return spec, fmt.Errorf("failed to ensure workspace assets: %w", err)
		}

		// Mount boot.sh as read-only into the container
		// Use :z for SELinux shared label (required for rootless podman on SELinux systems)
		spec.Volumes = append(spec.Volumes, container.VolumeMapping{
			Host:      BootShHostPath(),
			Container: "/piccolo/boot.sh",
			Options:   "ro,z",
		})

		// Mount piccolo-startup helper to /usr/local/bin (which is in PATH by default)
		spec.Volumes = append(spec.Volumes, container.VolumeMapping{
			Host:      PiccoloStartupHostPath(),
			Container: "/usr/local/bin/piccolo-startup",
			Options:   "ro,z",
		})

		// Mount a writable config directory for user startup hooks (start.sh)
		// This directory is persistent and writable by the container user
		configDir := filepath.Join(layout.DataDir, "piccolo-config")
		if err := os.MkdirAll(configDir, 0o777); err != nil {
			return spec, fmt.Errorf("failed to create piccolo config dir: %w", err)
		}
		spec.Volumes = append(spec.Volumes, container.VolumeMapping{
			Host:      configDir,
			Container: "/piccolo/config",
			Options:   "rw,U,z", // U for rootless UID mapping
		})
	}

	// Validate the container spec
	if err := container.ValidateContainerSpec(spec); err != nil {
		return spec, fmt.Errorf("invalid container spec: %w", err)
	}

	return spec, nil
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
// If service is empty, defaults to the primary service (or the single container for legacy apps).
func (m *AppManager) ExecShellCmdForService(ctx context.Context, instanceID, service string) (*exec.Cmd, error) {
	state, err := m.ensureStateManager()
	if err != nil {
		return nil, err
	}
	appInst, exists := state.GetApp(instanceID)
	if !exists {
		return nil, fmt.Errorf("app instance not found: %s", instanceID)
	}
	if appInst.ContainerID == "" {
		return nil, fmt.Errorf("app %s has no container ID", instanceID)
	}
	if appInst.Status != "running" {
		return nil, fmt.Errorf("app %s is not running (status: %s)", instanceID, appInst.Status)
	}

	layout, err := m.ensureAppVolumeLayout(ctx, instanceID)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve volume layout: %w", err)
	}
	runtime, err := m.podmanRuntimeForApp(instanceID, layout)
	if err != nil {
		return nil, fmt.Errorf("failed to create podman runtime: %w", err)
	}

	def := appInst.Definition
	if def != nil && def.Services != nil {
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
				if target == primary {
					appInst.ContainerID = id
				}
				_ = state.StoreApp(appInst, nil)
			}
		}
		if cid == "" {
			return nil, fmt.Errorf("container not found for service '%s'", target)
		}
		return m.containerManager.ExecShellCmd(runtime, cid)
	}

	return m.containerManager.ExecShellCmd(runtime, appInst.ContainerID)
}
