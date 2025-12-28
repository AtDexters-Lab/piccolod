package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
func workspaceSnapshotImage(appName string) string {
	return fmt.Sprintf("localhost/%s:snapshot", appName)
}

// NewAppManagerWithServices creates a new filesystem-based app manager with an injected ServiceManager
func NewAppManagerWithServices(containerManager ContainerManager, stateDir string, serviceManager *services.ServiceManager, lockReader LockStateReader) (*AppManager, error) {
	base := stateDir
	if strings.TrimSpace(base) == "" {
		base = paths.Root()
	}
	base = filepath.Clean(base)
	return &AppManager{
		containerManager: containerManager,
		stateBaseDir:     base,
		serviceManager:   serviceManager,
		leadershipState:  make(map[string]cluster.Role),
		lockReader:       lockReader,
		mountVerifier:    defaultMountVerifier,
	}, nil
}

// SetRouter wires the router registrar for leadership-based routing decisions.
func (m *AppManager) SetRouter(reg router.Registrar) {
	m.stateMu.Lock()
	m.routeRegistrar = reg
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
		if err := m.stopInternal(ctx, app.Name); err != nil {
			log.Printf("WARN: follower transition stop app %s failed: %v", app.Name, err)
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
		if app.ContainerID == "" {
			continue
		}
		// Respect desired state: stopped apps should not have proxies restored.
		if app.Status == "stopped" {
			if m.serviceManager != nil {
				m.serviceManager.RemoveApp(app.Name)
			}
			continue
		}
		// Followers should not restore proxies for apps they don't lead.
		if m.LastObservedRole(cluster.ResourceForApp(app.Name)) == cluster.RoleFollower {
			if m.serviceManager != nil {
				m.serviceManager.RemoveApp(app.Name)
			}
			continue
		}
		def, err := state.GetAppDefinition(app.Name)
		if err != nil {
			log.Printf("WARN: restore services: failed to read app definition for %s: %v", app.Name, err)
			continue
		}
		layout, err := m.ensureAppVolumeLayout(ctx, app.Name)
		if err != nil {
			log.Printf("WARN: restore services: app volume unavailable for %s: %v", app.Name, err)
			continue
		}
		runtime, err := m.podmanRuntimeForApp(app.Name, layout)
		if err != nil {
			log.Printf("WARN: restore services: podman runtime unavailable for %s: %v", app.Name, err)
			continue
		}
		ports, err := m.containerManager.InspectPublishedPorts(ctx, runtime, app.ContainerID)
		if err != nil {
			log.Printf("WARN: restore services: podman port inspect failed for %s: %v", app.Name, err)
			continue
		}
		if len(ports) == 0 {
			m.serviceManager.RemoveApp(app.Name)
			continue
		}
		if _, err := m.serviceManager.RestoreFromPodman(app.Name, def.Listeners, ports); err != nil {
			log.Printf("WARN: restore services: failed to restore proxies for %s: %v", app.Name, err)
			continue
		}
		m.serviceManager.SetAppContainerID(app.Name, app.ContainerID)
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
		if appInst == nil || strings.TrimSpace(appInst.Name) == "" {
			continue
		}
		if err := m.reconcileApp(ctx, state, appInst); err != nil {
			log.Printf("WARN: reconcile app %s: %v", appInst.Name, err)
		}
	}
}

func (m *AppManager) reconcileApp(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance) error {
	desiredRunning := appInst.Status != "stopped"
	if m.LastObservedRole(cluster.ResourceForApp(appInst.Name)) == cluster.RoleFollower {
		desiredRunning = false
	}

	layout, err := m.ensureAppVolumeLayout(ctx, appInst.Name)
	if err != nil {
		return err
	}
	runtime, err := m.podmanRuntimeForApp(appInst.Name, layout)
	if err != nil {
		return err
	}

	containerID := strings.TrimSpace(appInst.ContainerID)
	resolveID := func() (string, error) {
		id, err := m.containerManager.ResolveContainerIDByName(ctx, runtime, appInst.Name)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(id) == "" {
			return "", container.ErrContainerNotFound(appInst.Name)
		}
		return id, nil
	}

	if containerID == "" {
		if id, err := resolveID(); err == nil {
			containerID = id
			_ = state.UpdateAppRuntime(appInst.Name, appInst.Status, containerID)
			appInst.ContainerID = containerID
		}
	}

	var observed container.ContainerState
	if containerID != "" {
		observed, err = m.containerManager.InspectContainerState(ctx, runtime, containerID)
		if err != nil {
			log.Printf("WARN: reconcile app %s: inspect state failed: %v", appInst.Name, err)
			observed = container.ContainerState{}
		}
	}

	if containerID == "" || !observed.Exists {
		if id, err := resolveID(); err == nil && id != "" && id != containerID {
			containerID = id
			_ = state.UpdateAppRuntime(appInst.Name, appInst.Status, containerID)
			appInst.ContainerID = containerID
			observed, _ = m.containerManager.InspectContainerState(ctx, runtime, containerID)
		}
	}

	if containerID == "" || !observed.Exists {
		if !desiredRunning {
			if m.serviceManager != nil {
				m.serviceManager.RemoveApp(appInst.Name)
			}
			return nil
		}
		def, err := state.GetAppDefinition(appInst.Name)
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
			m.serviceManager.RemoveApp(appInst.Name)
		}
		return nil
	}

	// Desired running.
	if !observed.Running {
		if err := m.containerManager.StartContainer(ctx, runtime, containerID); err != nil {
			_ = state.UpdateAppStatus(appInst.Name, "error")
			return err
		}
	}
	if appInst.Status != "running" {
		_ = state.UpdateAppStatus(appInst.Name, "running")
	}

	def, err := state.GetAppDefinition(appInst.Name)
	if err != nil {
		return err
	}

	if m.serviceManager != nil {
		if err := m.ensureServicesForRunningApp(ctx, def, appInst.Name, containerID, runtime); err != nil {
			log.Printf("WARN: reconcile app %s: restore services failed: %v", appInst.Name, err)
		}
		if err := m.ensurePodmanPublishes(ctx, def, appInst.Name, containerID, runtime); err != nil {
			log.Printf("WARN: reconcile app %s: publish reconcile failed: %v", appInst.Name, err)
		}
	}
	return nil
}

func (m *AppManager) recreateMissingContainer(ctx context.Context, state *FilesystemStateManager, appInst *AppInstance, def *api.AppDefinition, layout appVolumeLayout, runtime container.PodmanRuntime) error {
	if m.serviceManager == nil {
		return fmt.Errorf("app manager: service manager not configured")
	}

	for attempt := 0; attempt < maxInstallPortRetries; attempt++ {
		endpoints, err := m.serviceManager.AllocateForApp(appInst.Name, def.Listeners)
		if err != nil {
			return fmt.Errorf("allocate service ports: %w", err)
		}
		spec, err := m.appDefToContainerSpec(def, endpoints, layout)
		if err != nil {
			m.serviceManager.RemoveApp(appInst.Name)
			return err
		}
		cid, err := m.containerManager.CreateContainer(ctx, runtime, spec)
		if err == nil {
			appInst.ContainerID = cid
			_ = state.UpdateAppRuntime(appInst.Name, "running", cid)
			m.serviceManager.SetAppContainerID(appInst.Name, cid)
			return nil
		}

		var nameErr *container.NameInUseError
		if errors.As(err, &nameErr) {
			log.Printf("INFO: recreate app %s: adopted existing container %s", appInst.Name, nameErr.ID)
			// Discard speculative port allocation; ensureServicesForRunningApp will restore actual ports.
			m.serviceManager.RemoveApp(appInst.Name)
			appInst.ContainerID = nameErr.ID
			_ = state.UpdateAppRuntime(appInst.Name, appInst.Status, nameErr.ID)
			return nil
		}

		var portErr *container.PortInUseError
		if errors.As(err, &portErr) {
			log.Printf("WARN: recreate app %s: host port conflict port=%d attempt=%d", appInst.Name, portErr.Port, attempt)
			if portErr.Port > 0 {
				_ = m.serviceManager.ReserveHostPort(portErr.Port)
			} else {
				for _, ep := range endpoints {
					_ = m.serviceManager.ReserveHostPort(ep.HostBind)
				}
			}
			m.serviceManager.RemoveApp(appInst.Name)
			continue
		}

		m.serviceManager.RemoveApp(appInst.Name)
		return err
	}

	return fmt.Errorf("failed to recreate %s: exhausted host-port retries", appInst.Name)
}

func (m *AppManager) ensureServicesForRunningApp(ctx context.Context, def *api.AppDefinition, appName, containerID string, runtime container.PodmanRuntime) error {
	if m.serviceManager == nil {
		return nil
	}
	if _, err := m.serviceManager.GetByApp(appName); err == nil {
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
		if _, err := m.serviceManager.AllocateForApp(appName, def.Listeners); err != nil {
			return err
		}
		m.serviceManager.SetAppContainerID(appName, containerID)
		return nil
	}

	if _, err := m.serviceManager.RestoreFromPodman(appName, def.Listeners, ports); err != nil {
		return err
	}
	m.serviceManager.SetAppContainerID(appName, containerID)
	return nil
}

func (m *AppManager) ensurePodmanPublishes(ctx context.Context, def *api.AppDefinition, appName, containerID string, runtime container.PodmanRuntime) error {
	if m.serviceManager == nil {
		return nil
	}
	if def == nil {
		return fmt.Errorf("app manager: app definition required to reconcile publishes")
	}
	endpoints, err := m.serviceManager.GetByApp(appName)
	if err != nil {
		return nil
	}

	for attempt := 0; attempt < maxInstallPortRetries; attempt++ {
		observed, err := m.containerManager.InspectPublishedPorts(ctx, runtime, containerID)
		if err != nil {
			return err
		}

		expected := make(map[int]services.ServiceEndpoint, len(endpoints)) // guest -> endpoint
		for _, ep := range endpoints {
			expected[ep.GuestPort] = ep
		}

		retry := false
		applyAdd := func(ep services.ServiceEndpoint) error {
			if err := m.containerManager.UpdatePublishAdd(ctx, runtime, containerID, ep.HostBind, ep.GuestPort); err != nil {
				var portErr *container.PortInUseError
				if errors.As(err, &portErr) {
					conflict := ep.HostBind
					if portErr.Port > 0 {
						conflict = portErr.Port
					}
					m.serviceManager.RemoveApp(appName)
					if conflict > 0 {
						_ = m.serviceManager.ReserveHostPort(conflict)
					}
					// Re-allocate endpoints and retry with new host ports.
					endpoints, err = m.serviceManager.AllocateForApp(appName, def.Listeners)
					if err != nil {
						return err
					}
					m.serviceManager.SetAppContainerID(appName, containerID)
					retry = true
					return nil
				}
				return err
			}
			return nil
		}

		// Ensure required mappings exist and match.
		for guest, ep := range expected {
			host, ok := observed[guest]
			switch {
			case !ok:
				if err := applyAdd(ep); err != nil {
					return err
				}
			case host != ep.HostBind:
				if err := applyAdd(ep); err != nil {
					return err
				}
				if retry {
					break
				}
				if err := m.containerManager.UpdatePublishRemove(ctx, runtime, containerID, host, guest); err != nil {
					log.Printf("WARN: reconcile app %s: publish remove failed: %v", appName, err)
				}
			}
			if retry {
				break
			}
		}

		if retry {
			continue
		}

		// Remove mappings for guest ports that are no longer expected.
		for guest, host := range observed {
			if _, ok := expected[guest]; !ok {
				if err := m.containerManager.UpdatePublishRemove(ctx, runtime, containerID, host, guest); err != nil {
					log.Printf("WARN: reconcile app %s: publish remove failed: %v", appName, err)
				}
			}
		}
		return nil
	}

	return fmt.Errorf("failed to reconcile publishes for %s: exhausted host-port retries", appName)
}

// Install installs a new application from its definition
func (m *AppManager) Install(ctx context.Context, appDef *api.AppDefinition) (*AppInstance, error) {
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()
	return m.installLocked(ctx, appDef)
}

func (m *AppManager) installLocked(ctx context.Context, appDef *api.AppDefinition) (*AppInstance, error) {
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

	// Check if app already exists
	if _, exists := state.GetApp(appDef.Name); exists {
		return nil, fmt.Errorf("app already exists: %s", appDef.Name)
	}

	return m.installWithRetries(ctx, state, appDef, 0)
}

func (m *AppManager) installWithRetries(ctx context.Context, state *FilesystemStateManager, appDef *api.AppDefinition, attempt int) (*AppInstance, error) {
	if attempt >= maxInstallPortRetries {
		return nil, fmt.Errorf("failed to install %s: exhausted host-port retries", appDef.Name)
	}

	layout, err := m.ensureAppVolumeLayout(ctx, appDef.Name)
	if err != nil {
		return nil, err
	}
	runtime, err := m.podmanRuntimeForApp(appDef.Name, layout)
	if err != nil {
		return nil, err
	}

	// Allocate services and convert to container spec
	endpoints, err := m.serviceManager.AllocateForApp(appDef.Name, appDef.Listeners)
	if err != nil {
		return nil, fmt.Errorf("failed to allocate service ports: %w", err)
	}
	cleanupServices := true
	defer func() {
		if cleanupServices {
			m.serviceManager.RemoveApp(appDef.Name)
		}
	}()

	containerSpec, err := m.appDefToContainerSpec(appDef, endpoints, layout)
	if err != nil {
		return nil, fmt.Errorf("failed to create container spec: %w", err)
	}

	// For workspace mode, check if a snapshot image exists from a previous uninstall.
	// If so, use the snapshot to restore the previous container filesystem state.
	mode := piccoloModeFromExtensions(appDef.Extensions)
	useSnapshot := false
	if mode == ModeWorkspace {
		snapshotImage := workspaceSnapshotImage(appDef.Name)
		exists, err := m.containerManager.ImageExists(ctx, runtime, snapshotImage)
		if err != nil {
			log.Printf("WARN: install %s: failed to check snapshot image: %v", appDef.Name, err)
		} else if exists {
			log.Printf("INFO: install %s: using workspace snapshot %s", appDef.Name, snapshotImage)
			containerSpec.Image = snapshotImage
			useSnapshot = true
		}
	}

	// Pull image to app's isolated storage to detect download issues early
	// Skip pull if using a local snapshot image
	if !useSnapshot {
		if err := m.containerManager.PullImage(ctx, runtime, appDef.Image); err != nil {
			log.Printf("WARN: install %s: image pull failed: %v", appDef.Name, err)
			// Proceeding anyway as CreateContainer might succeed if local, or fail with same error
		}
	}

	// Create container with zombie cleanup
	var containerID string
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
		} else if id, resolveErr := m.containerManager.ResolveContainerIDByName(ctx, runtime, appDef.Name); resolveErr == nil {
			zombieID = id
		}

		if zombieID != "" {
			log.Printf("INFO: install %s: removing zombie container %s", appDef.Name, zombieID)
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
			m.serviceManager.RemoveApp(appDef.Name)
			log.Printf("WARN: retrying install for %s due to host port conflict port=%d attempt=%d", appDef.Name, portErr.Port, attempt)
			if portErr.Port > 0 {
				_ = m.serviceManager.ReserveHostPort(portErr.Port)
			} else {
				for _, ep := range endpoints {
					_ = m.serviceManager.ReserveHostPort(ep.HostBind)
				}
			}
			return m.installWithRetries(ctx, state, appDef, attempt+1)
		}

		return nil, fmt.Errorf("failed to create container: %w", err)
	}

	// Start container immediately
	if err := m.containerManager.StartContainer(ctx, runtime, containerID); err != nil {
		// Atomic install: if start fails, cleanup and fail.
		// We do NOT persist the app state, so the user can retry.
		_ = m.containerManager.RemoveContainer(ctx, runtime, containerID)
		// Defer will cleanup services
		return nil, fmt.Errorf("failed to start container: %w", err)
	}

	// Record container ID for watcher reconciliation
	if m.serviceManager != nil {
		m.serviceManager.SetAppContainerID(appDef.Name, containerID)
	}

	// Create app instance
	now := time.Now()
	app := &AppInstance{
		Name:        appDef.Name,
		Image:       appDef.Image,
		Type:        appDef.Type,
		Status:      "running",
		ContainerID: containerID,
		Environment: appDef.Environment,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	// Store app to filesystem
	if err := state.StoreApp(app, appDef); err != nil {
		// Cleanup container if storage fails
		_ = m.containerManager.StopContainer(ctx, runtime, containerID)
		_ = m.containerManager.RemoveContainer(ctx, runtime, containerID)
		m.serviceManager.RemoveApp(appDef.Name)
		cleanupServices = false
		return nil, fmt.Errorf("failed to store app: %w", err)
	}

	cleanupServices = false

	return app, nil
}

// Upsert installs or updates an application by name. If the app exists, it is uninstalled and reinstalled.
func (m *AppManager) Upsert(ctx context.Context, appDef *api.AppDefinition) (*AppInstance, error) {
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()
	return m.upsertLocked(ctx, appDef)
}

func (m *AppManager) upsertLocked(ctx context.Context, appDef *api.AppDefinition) (*AppInstance, error) {
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
	if existing, exists := state.GetApp(appDef.Name); exists {
		layout, err := m.ensureAppVolumeLayout(ctx, appDef.Name)
		if err != nil {
			return nil, err
		}
		runtime, err := m.podmanRuntimeForApp(appDef.Name, layout)
		if err != nil {
			return nil, err
		}

		// Reconcile listeners first
		rec, containerChange, err := m.serviceManager.Reconcile(appDef.Name, appDef.Listeners)
		if err != nil {
			return nil, fmt.Errorf("failed to reconcile services: %w", err)
		}

		// Try in-place publish updates via Podman for adds/removes/guest port changes
		// Added
		for _, ep := range rec.Added {
			_ = m.containerManager.UpdatePublishAdd(ctx, runtime, existing.ContainerID, ep.HostBind, ep.GuestPort)
		}
		// Guest port changes: add new mapping, then remove old
		for _, ch := range rec.GuestPortChanged {
			_ = m.containerManager.UpdatePublishAdd(ctx, runtime, existing.ContainerID, ch.New.HostBind, ch.New.GuestPort)
			_ = m.containerManager.UpdatePublishRemove(ctx, runtime, existing.ContainerID, ch.Old.HostBind, ch.Old.GuestPort)
		}
		// Removed
		for _, ep := range rec.Removed {
			_ = m.containerManager.UpdatePublishRemove(ctx, runtime, existing.ContainerID, ep.HostBind, ep.GuestPort)
		}

		if containerChange {
			// If some podman updates failed silently, a full recreate could be a fallback in future.
		}

		// Persist new app.yaml and metadata
		if err := state.StoreApp(existing, appDef); err != nil {
			return nil, fmt.Errorf("failed to store app: %w", err)
		}
		return existing, nil
	}
	return m.installLocked(ctx, appDef)
}

// List returns all installed applications
func (m *AppManager) List(ctx context.Context) ([]*AppInstance, error) {
	state, err := m.ensureStateManager()
	if err != nil {
		return nil, err
	}
	return state.ListApps(), nil
}

// Get returns a specific application by name
func (m *AppManager) Get(ctx context.Context, name string) (*AppInstance, error) {
	state, err := m.ensureStateManager()
	if err != nil {
		return nil, err
	}
	app, exists := state.GetApp(name)
	if !exists {
		return nil, fmt.Errorf("app not found: %s", name)
	}

	return app, nil
}

// GetAppDefinition returns the full definition (app.yaml content) for an installed app.
func (m *AppManager) GetAppDefinition(ctx context.Context, name string) (*api.AppDefinition, error) {
	state, err := m.ensureStateManager()
	if err != nil {
		return nil, err
	}
	return state.GetAppDefinition(name)
}

// Start starts an application
func (m *AppManager) Start(ctx context.Context, name string) error {
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()
	return m.startLocked(ctx, name)
}

func (m *AppManager) startLocked(ctx context.Context, name string) error {
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
	app, exists := state.GetApp(name)
	if !exists {
		return fmt.Errorf("app not found: %s", name)
	}

	layout, err := m.ensureAppVolumeLayout(ctx, name)
	if err != nil {
		return err
	}
	runtime, err := m.podmanRuntimeForApp(name, layout)
	if err != nil {
		return err
	}

	// Start the container
	if err := m.containerManager.StartContainer(ctx, runtime, app.ContainerID); err != nil {
		// Update status to error
		_ = state.UpdateAppStatus(name, "error")
		return fmt.Errorf("failed to start container: %w", err)
	}

	// Update status to running
	if err := state.UpdateAppStatus(name, "running"); err != nil {
		return fmt.Errorf("failed to update app status: %w", err)
	}

	// Rehydrate service proxies if they were removed while the app was stopped
	if _, err := m.serviceManager.GetByApp(name); err != nil {
		def, defErr := state.GetAppDefinition(name)
		if defErr != nil {
			log.Printf("WARN: start app %s: failed to load app definition: %v", name, defErr)
		} else {
			ports, portErr := m.containerManager.InspectPublishedPorts(ctx, runtime, app.ContainerID)
			if portErr != nil {
				log.Printf("WARN: start app %s: inspect ports failed: %v", name, portErr)
			} else if len(ports) == 0 {
				log.Printf("WARN: start app %s: no published ports found during restore", name)
			} else {
				if _, restoreErr := m.serviceManager.RestoreFromPodman(name, def.Listeners, ports); restoreErr != nil {
					log.Printf("WARN: start app %s: failed to restore services: %v", name, restoreErr)
				} else {
					m.serviceManager.SetAppContainerID(name, app.ContainerID)
				}
			}
		}
	}

	return nil
}

// Stop stops an application
func (m *AppManager) Stop(ctx context.Context, name string) error {
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()

	if err := m.ensureUnlocked(); err != nil {
		return err
	}
	if err := m.ensureKernelLeader(); err != nil {
		return err
	}
	return m.stopInternal(ctx, name)
}

func (m *AppManager) stopForFollowerTransition(ctx context.Context, name string) error {
	state, err := m.ensureStateManager()
	if err != nil {
		return err
	}
	app, exists := state.GetApp(name)
	if !exists {
		return nil
	}

	layout, err := m.ensureAppVolumeLayout(ctx, name)
	if err != nil {
		return err
	}
	runtime, err := m.podmanRuntimeForApp(name, layout)
	if err != nil {
		return err
	}

	if err := m.containerManager.StopContainer(ctx, runtime, app.ContainerID); err != nil {
		var notFound *container.ContainerNotFoundError
		if !errors.As(err, &notFound) {
			return err
		}
	}
	if m.serviceManager != nil {
		m.serviceManager.RemoveApp(name)
	}
	return nil
}

func (m *AppManager) stopInternal(ctx context.Context, name string) error {
	state, err := m.ensureStateManager()
	if err != nil {
		return err
	}
	app, exists := state.GetApp(name)
	if !exists {
		return fmt.Errorf("app not found: %s", name)
	}

	layout, err := m.ensureAppVolumeLayout(ctx, name)
	if err != nil {
		return err
	}
	runtime, err := m.podmanRuntimeForApp(name, layout)
	if err != nil {
		return err
	}

	if err := m.containerManager.StopContainer(ctx, runtime, app.ContainerID); err != nil {
		_ = state.UpdateAppStatus(name, "error")
		return fmt.Errorf("failed to stop container: %w", err)
	}

	if err := state.UpdateAppStatus(name, "stopped"); err != nil {
		return fmt.Errorf("failed to update app status: %w", err)
	}

	if m.serviceManager != nil {
		m.serviceManager.RemoveApp(name)
	}

	return nil
}

// Uninstall removes an application completely
func (m *AppManager) Uninstall(ctx context.Context, name string) error {
	if err := m.ensureUnlocked(); err != nil {
		return err
	}
	if err := m.ensureKernelLeader(); err != nil {
		return err
	}
	return m.UninstallWithOptions(ctx, name, false)
}

// UninstallWithOptions removes an application; when purge is true, also deletes app data directories
func (m *AppManager) UninstallWithOptions(ctx context.Context, name string, purge bool) error {
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()
	return m.uninstallLocked(ctx, name, purge)
}

func (m *AppManager) uninstallLocked(ctx context.Context, name string, purge bool) error {
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
	app, exists := state.GetApp(name)
	if !exists {
		return fmt.Errorf("app not found: %s", name)
	}

	layout, err := m.ensureAppVolumeLayout(ctx, name)
	if err != nil {
		return err
	}
	runtime, err := m.podmanRuntimeForApp(name, layout)
	if err != nil {
		return err
	}

	// Stop container first (ignore error if already stopped)
	_ = m.containerManager.StopContainer(ctx, runtime, app.ContainerID)

	// For workspace mode without purge, commit container state to snapshot image.
	// This preserves the container filesystem so it can be restored on reinstall.
	if !purge {
		def, defErr := state.GetAppDefinition(name)
		if defErr == nil {
			mode := piccoloModeFromExtensions(def.Extensions)
			if mode == ModeWorkspace && app.ContainerID != "" {
				snapshotImage := workspaceSnapshotImage(name)
				if err := m.containerManager.CommitContainer(ctx, runtime, app.ContainerID, snapshotImage); err != nil {
					log.Printf("WARN: workspace %s: failed to commit snapshot: %v", name, err)
				} else {
					log.Printf("INFO: workspace %s: committed snapshot to %s", name, snapshotImage)
				}
			}
		}
	}

	// Remove container
	if err := m.containerManager.RemoveContainer(ctx, runtime, app.ContainerID); err != nil {
		return fmt.Errorf("failed to remove container: %w", err)
	}

	// Stop and remove service listeners for this app
	if m.serviceManager != nil {
		m.serviceManager.RemoveApp(name)
	}

	// Optionally purge app data (destroy volume and podman runtime state)
	if purge {
		// Reset podman storage to clean up any remaining containers
		if err := m.containerManager.ResetStorage(ctx, runtime); err != nil {
			log.Printf("WARN: podman storage reset for %s failed: %v", name, err)
		}

		volID := appVolumeID(name)
		if err := m.volumeManager.DestroyVolume(ctx, volID); err != nil {
			return fmt.Errorf("failed to purge app data: %w", err)
		}

		// Remove podman runRoot which lives outside the encrypted volume
		if err := os.RemoveAll(runtime.RunRoot); err != nil {
			log.Printf("WARN: failed to remove podman runRoot %s: %v", runtime.RunRoot, err)
		}
	}

	// Remove from filesystem and cache (state only)
	if err := state.RemoveApp(name); err != nil {
		return fmt.Errorf("failed to remove app from storage: %w", err)
	}

	return nil
}

// Enable enables an application (systemctl-style)
func (m *AppManager) Enable(ctx context.Context, name string) error {
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
	if _, exists := state.GetApp(name); !exists {
		return fmt.Errorf("app not found: %s", name)
	}

	return state.EnableApp(name)
}

// Disable disables an application (systemctl-style)
func (m *AppManager) Disable(ctx context.Context, name string) error {
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
	if _, exists := state.GetApp(name); !exists {
		return fmt.Errorf("app not found: %s", name)
	}

	return state.DisableApp(name)
}

// IsEnabled checks if an application is enabled
func (m *AppManager) IsEnabled(ctx context.Context, name string) (bool, error) {
	state, err := m.ensureStateManager()
	if err != nil {
		return false, err
	}
	if _, exists := state.GetApp(name); !exists {
		return false, fmt.Errorf("app not found: %s", name)
	}

	return state.IsAppEnabled(name), nil
}

// ListEnabled returns names of all enabled apps
func (m *AppManager) ListEnabled(ctx context.Context) ([]string, error) {
	state, err := m.ensureStateManager()
	if err != nil {
		return nil, err
	}
	return state.ListEnabledApps()
}

// UpdateImage updates an app's container image tag and recreates the container preserving services
func (m *AppManager) UpdateImage(ctx context.Context, name string, tag *string) error {
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()
	return m.updateImageLocked(ctx, name, tag)
}

func (m *AppManager) updateImageLocked(ctx context.Context, name string, tag *string) error {
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
	appInst, exists := state.GetApp(name)
	if !exists {
		return fmt.Errorf("app not found: %s", name)
	}
	layout, err := m.ensureAppVolumeLayout(ctx, name)
	if err != nil {
		return err
	}
	runtime, err := m.podmanRuntimeForApp(name, layout)
	if err != nil {
		return err
	}
	// Load current app definition
	curDef, err := state.GetAppDefinition(name)
	if err != nil {
		return fmt.Errorf("failed to read current app.yaml: %w", err)
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
	if err := state.BackupCurrentAppDefinition(name); err != nil {
		return fmt.Errorf("backup app.yaml: %w", err)
	}
	// Pull image to app's storage (best effort)
	_ = m.containerManager.PullImage(ctx, runtime, newImage)
	// Preserve endpoints
	endpoints, _ := m.serviceManager.GetByApp(name)
	// Stop and remove old container
	_ = m.containerManager.StopContainer(ctx, runtime, appInst.ContainerID)
	_ = m.containerManager.RemoveContainer(ctx, runtime, appInst.ContainerID)
	// Create new container with same endpoints
	spec, err := m.appDefToContainerSpec(&newDef, endpoints, layout)
	if err != nil {
		return fmt.Errorf("build container spec: %w", err)
	}
	newCID, err := m.containerManager.CreateContainer(ctx, runtime, spec)
	if err != nil {
		return fmt.Errorf("create container: %w", err)
	}
	if m.serviceManager != nil {
		m.serviceManager.SetAppContainerID(name, newCID)
	}
	// Update instance and persist app.yaml + metadata
	appInst.Image = newImage
	appInst.ContainerID = newCID
	appInst.Status = "created"
	appInst.UpdatedAt = time.Now()
	if err := state.StoreApp(appInst, &newDef); err != nil {
		return fmt.Errorf("store app: %w", err)
	}
	return nil
}

// Revert reverts an app to the previous app.yaml (if available) and recreates container
func (m *AppManager) Revert(ctx context.Context, name string) error {
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()
	return m.revertLocked(ctx, name)
}

func (m *AppManager) revertLocked(ctx context.Context, name string) error {
	if err := m.ensureUnlocked(); err != nil {
		return err
	}
	state, err := m.ensureStateManager()
	if err != nil {
		return err
	}
	appInst, exists := state.GetApp(name)
	if !exists {
		return fmt.Errorf("app not found: %s", name)
	}
	layout, err := m.ensureAppVolumeLayout(ctx, name)
	if err != nil {
		return err
	}
	runtime, err := m.podmanRuntimeForApp(name, layout)
	if err != nil {
		return err
	}
	// Read previous def
	prevDef, err := state.GetPreviousAppDefinition(name)
	if err != nil {
		return fmt.Errorf("no previous version to revert to: %w", err)
	}
	// Backup current before writing previous
	if err := state.BackupCurrentAppDefinition(name); err != nil {
		return fmt.Errorf("backup current: %w", err)
	}
	// Preserve endpoints
	endpoints, _ := m.serviceManager.GetByApp(name)
	// Stop and remove current container
	_ = m.containerManager.StopContainer(ctx, runtime, appInst.ContainerID)
	_ = m.containerManager.RemoveContainer(ctx, runtime, appInst.ContainerID)
	// Pull to app's storage (best-effort)
	if prevDef.Image != "" {
		_ = m.containerManager.PullImage(ctx, runtime, prevDef.Image)
	}
	// Create new container from prev
	spec, err := m.appDefToContainerSpec(prevDef, endpoints, layout)
	if err != nil {
		return fmt.Errorf("build container spec: %w", err)
	}
	newCID, err := m.containerManager.CreateContainer(ctx, runtime, spec)
	if err != nil {
		return fmt.Errorf("create container: %w", err)
	}
	if m.serviceManager != nil {
		m.serviceManager.SetAppContainerID(name, newCID)
	}
	// Update instance and persist prev as current
	appInst.Image = prevDef.Image
	appInst.ContainerID = newCID
	appInst.Status = "created"
	appInst.UpdatedAt = time.Now()
	if err := state.StoreApp(appInst, prevDef); err != nil {
		return fmt.Errorf("store app: %w", err)
	}
	return nil
}

// Logs fetches recent container logs for an app
func (m *AppManager) Logs(ctx context.Context, name string, lines int) ([]string, error) {
	state, err := m.ensureStateManager()
	if err != nil {
		return nil, err
	}
	appInst, exists := state.GetApp(name)
	if !exists {
		return nil, fmt.Errorf("app not found: %s", name)
	}
	if lines <= 0 {
		lines = 200
	}
	layout, err := m.ensureAppVolumeLayout(ctx, name)
	if err != nil {
		return nil, err
	}
	runtime, err := m.podmanRuntimeForApp(name, layout)
	if err != nil {
		return nil, err
	}
	return m.containerManager.Logs(ctx, runtime, appInst.ContainerID, lines)
}

// appDefToContainerSpec converts an AppDefinition to a ContainerCreateSpec.
// storage volumes are mapped into the per-app encrypted volume at <mount>/data/<volume-name>.
func (m *AppManager) appDefToContainerSpec(appDef *api.AppDefinition, endpoints []services.ServiceEndpoint, layout appVolumeLayout) (container.ContainerCreateSpec, error) {
	spec := container.ContainerCreateSpec{
		Name:        appDef.Name,
		Image:       appDef.Image,
		Environment: appDef.Environment,
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
	if layout.DataDir == "" {
		return spec, fmt.Errorf("container spec requires app volume layout")
	}
	mountedPaths := map[string]struct{}{}
	if appDef.Storage != nil {
		for volName, vol := range appDef.Storage.Persistent {
			host := filepath.Join(layout.DataDir, volName)
			if err := ensureDir(host, 0o777); err != nil {
				return spec, fmt.Errorf("ensure persistent volume '%s': %w", volName, err)
			}
			spec.Volumes = append(spec.Volumes, container.VolumeMapping{
				Host:      host,
				Container: vol.Container,
				Options:   "rw,U",
			})
			mountedPaths[vol.Container] = struct{}{}
		}
		for _, vol := range appDef.Storage.Temporary {
			mountedPaths[vol.Container] = struct{}{}
		}
	}

	// Canonical tmpfs mounts: honor x-piccolo.tmpfs when present, otherwise defaults.
	for _, p := range tmpfsMountsFromExtensions(appDef.Extensions) {
		if _, ok := mountedPaths[p]; ok {
			continue
		}
		spec.Tmpfs = append(spec.Tmpfs, container.TmpfsMount{Container: p})
	}

	if appDef.Storage != nil {
		for _, vol := range appDef.Storage.Temporary {
			opts := "rw"
			if vol.SizeLimit != "" {
				if sizeOpt := tmpfsSizeOpt(vol.SizeLimit); sizeOpt != "" {
					opts = opts + "," + sizeOpt
				}
			}
			spec.Tmpfs = append(spec.Tmpfs, container.TmpfsMount{
				Container: vol.Container,
				Options:   opts,
			})
		}
	}

	// Validate the container spec
	if err := container.ValidateContainerSpec(spec); err != nil {
		return spec, fmt.Errorf("invalid container spec: %w", err)
	}

	return spec, nil
}

// purgeAppData removes all app data within the per-app encrypted volume.
// This is best-effort and is only intended to run when uninstalling with purge=true.
func (m *AppManager) purgeAppData(ctx context.Context, name string) error {
	if err := m.ensureUnlocked(); err != nil {
		return err
	}
	layout, err := m.ensureAppVolumeLayout(ctx, name)
	if err != nil {
		return err
	}
	_ = os.RemoveAll(filepath.Join(layout.MountDir, "data"))
	_ = os.RemoveAll(filepath.Join(layout.MountDir, "disk"))
	return nil
}
