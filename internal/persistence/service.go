package persistence

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"piccolod/internal/cluster"
	"piccolod/internal/crypt"
	"piccolod/internal/events"
	"piccolod/internal/runner"
	"piccolod/internal/runtime/commands"
	"piccolod/internal/state/paths"
	"piccolod/internal/storage/drbd"
	"piccolod/internal/storage/lvm"
	"piccolod/internal/storage/nbd"
)

// Options captures construction parameters for the persistence service.
type Options struct {
	Control        ControlStore
	Volumes        VolumeManager
	Devices        DeviceManager
	StorageAdapter StorageAdapter
	Consensus      ConsensusManager
	Events         *events.Bus
	Leadership     *cluster.Registry
	Dispatcher     *commands.Dispatcher
	Crypto         *crypt.Manager
	StateDir       string
	DataDir        string

	// Block-native storage dependencies (for luksVolumeManager).
	Runner  runner.CommandRunner
	LVMgr   *lvm.LVManager
	PoolMgr *lvm.PoolManager
	NBDSrv  *nbd.Server
	DRBDMgr *drbd.ResourceManager

	// Rootfs dependencies.
	FlattenFn   func(ctx context.Context, imageRef, targetDir, prePulledDir string) (GoldenImageConfig, error)
	ImageSizeFn func(ctx context.Context, imageRef string) (int64, error)
}

// Module implements the Service interface using pluggable sub-components.
type Module struct {
	control            ControlStore
	volumes            VolumeManager
	rootfs             RootfsVolumeManager
	devices            DeviceManager
	events             *events.Bus
	leadership         *cluster.Registry
	storage            StorageAdapter
	consensus          ConsensusManager
	crypto             *crypt.Manager
	stateDir           string
	controlHandle      VolumeHandle
	commitMu           sync.Mutex
	lastCommitRevision uint64
	pollCancel         context.CancelFunc
	leadershipCancel   func()
	lockOpMu           sync.Mutex
	lockStateMu        sync.RWMutex
	lockState          bool
	healthMu           sync.Mutex
	healthCancel       context.CancelFunc
	healthInterval     time.Duration
}

// Ensure Module satisfies the Service interface.
var _ Service = (*Module)(nil)

// NewService builds a persistence module with no-op implementations. Concrete
// components can be supplied by replacing the defaults on the returned module.
func NewService(opts Options) (*Module, error) {
	stateDir := opts.StateDir
	if stateDir == "" {
		stateDir = paths.CoreRoot()
	}
	if err := os.MkdirAll(stateDir, 0o711); err != nil {
		return nil, err
	}
	// Ensure core root is world-traversable so per-app users can reach their
	// mount points. MkdirAll is a no-op on existing dirs, so chmod explicitly.
	if err := os.Chmod(stateDir, 0o711); err != nil {
		return nil, fmt.Errorf("chmod core root: %w", err)
	}
	mod := &Module{
		control:        opts.Control,
		volumes:        opts.Volumes,
		devices:        opts.Devices,
		storage:        opts.StorageAdapter,
		consensus:      opts.Consensus,
		events:         opts.Events,
		leadership:     opts.Leadership,
		crypto:         opts.Crypto,
		stateDir:       stateDir,
		lockState:      true,
		healthInterval: 5 * time.Minute,
	}

	if mod.events == nil {
		mod.events = events.NewBus()
	}
	if mod.leadership == nil {
		mod.leadership = cluster.NewRegistry()
	}
	if mod.control == nil {
		if mod.crypto == nil {
			return nil, ErrCryptoUnavailable
		}
		store, err := newControlStore(mod.stateDir, mod.crypto)
		if err != nil {
			return nil, err
		}
		mod.control = newGuardedControlStore(store, func() bool {
			if mod.leadership == nil {
				return true
			}
			return mod.leadership.Current(cluster.ResourceKernel) != cluster.RoleFollower
		}, mod.onControlCommit)
	}
	if mod.volumes == nil {
		if mod.crypto == nil {
			return nil, ErrCryptoUnavailable
		}
		run := opts.Runner
		if run == nil {
			run = runner.ExecRunner{}
		}
		lvm, err := NewLUKSVolumeManager(LUKSVolumeManagerConfig{
			Run:       run,
			Crypto:    mod.crypto,
			Bus:       mod.events,
			LVMgr:     opts.LVMgr,
			PoolMgr:   opts.PoolMgr,
			NBDSrv:    opts.NBDSrv,
			DRBDMgr:   opts.DRBDMgr,
			FlattenFn:   opts.FlattenFn,
			ImageSizeFn: opts.ImageSizeFn,
		})
		if err != nil {
			return nil, err
		}
		mod.volumes = lvm
		// Only expose rootfs capabilities when FlattenFn is available.
		if opts.FlattenFn != nil {
			mod.rootfs = lvm
		}
	}
	if rc, ok := mod.volumes.(RoleCheckable); ok {
		rc.SetRoleChecker(func(volumeID string, role VolumeRole) bool {
			if role != VolumeRoleLeader {
				return true
			}
			if volumeID == "control-plane" {
				if mod.leadership == nil {
					return false
				}
				return mod.leadership.Current(cluster.ResourceKernel) == cluster.RoleLeader
			}
			return true
		})
	}
	if mod.devices == nil {
		mod.devices = newNoopDeviceManager()
	}
	if mod.storage == nil {
		mod.storage = newNoopStorageAdapter()
	}
	if mod.consensus == nil {
		mod.consensus = newNoopConsensusManager()
	}

	mod.observeLeadership()

	// Seed the canonical control handle BEFORE the initial setLockState
	// call. ensureCoreVolumes (further down) will overwrite this with the
	// EnsureVolume return value, but the value is structurally identical:
	// id is "control-plane" by convention, MountDir is derived from id.
	// Without this seed, the very first setLockState(true) sees a zero
	// VolumeHandle, classifyForDetach returns NoOp, and a stale post-crash
	// control mapper survives across "logically locked" — codex iter-7 B1.
	mod.controlHandle = VolumeHandle{ID: "control-plane", MountDir: paths.MountDir("control-plane")}

	if err := mod.setLockState(context.Background(), true); err != nil {
		return nil, err
	}
	mod.publishLockState(true)

	if err := mod.ensureCoreVolumes(context.Background()); err != nil {
		return nil, err
	}
	mod.startRevisionPoller()
	mod.startControlHealthMonitor()

	if opts.Dispatcher != nil {
		mod.registerHandlers(opts.Dispatcher)
	}

	return mod, nil
}

func (m *Module) ensureCoreVolumes(ctx context.Context) error {
	if m.volumes == nil {
		return nil
	}
	if r, ok := m.volumes.(Reconcilable); ok {
		if err := r.ReconcileAllVolumeStates(); err != nil {
			return err
		}
	}
	controlReq := VolumeRequest{ID: "control-plane", Class: VolumeClassControl, ClusterMode: ClusterModeStateful}
	if handle, err := m.volumes.EnsureVolume(ctx, controlReq); err != nil {
		return err
	} else {
		m.controlHandle = handle
	}
	return nil
}

func (m *Module) attachControlVolume(ctx context.Context) error {
	if m.volumes == nil {
		return nil
	}
	if m.controlHandle.ID == "" {
		return fmt.Errorf("control volume handle unavailable")
	}
	// Re-ensure the control volume: on a fresh system, the initial EnsureVolume
	// during startup defers creation (crypto not ready). Now that crypto is
	// initialized and unlocked, this creates the LUKS loop volume if needed.
	controlReq := VolumeRequest{ID: "control-plane", Class: VolumeClassControl, ClusterMode: ClusterModeStateful}
	if handle, err := m.volumes.EnsureVolume(ctx, controlReq); err != nil {
		return fmt.Errorf("ensure control volume: %w", err)
	} else {
		m.controlHandle = handle
	}
	role := VolumeRoleLeader
	if m.leadership != nil {
		if current := m.leadership.Current(cluster.ResourceKernel); current == cluster.RoleFollower {
			role = VolumeRoleFollower
		}
	}
	attachCtx := context.WithoutCancel(ctx)
	if err := m.volumes.Attach(attachCtx, m.controlHandle, AttachOptions{Role: role}); err != nil {
		if errors.Is(err, ErrNotImplemented) {
			return nil
		}
		return err
	}
	return nil
}

// registerHandlers wires persistence commands into the dispatcher.
func (m *Module) registerHandlers(dispatcher *commands.Dispatcher) {
	dispatcher.Register(CommandEnsureVolume, commands.HandlerFunc(m.handleEnsureVolume))
	dispatcher.Register(CommandAttachVolume, commands.HandlerFunc(m.handleAttachVolume))
	dispatcher.Register(CommandRecordLockState, commands.HandlerFunc(m.handleRecordLockState))
}

type lockableControlStore interface {
	Lock()
	Unlock(context.Context) error
}

func (m *Module) observeLeadership() {
	if m.events == nil {
		return
	}
	ch, cancel := m.events.SubscribeWithCancel(events.TopicLeadershipRoleChanged, 8)
	m.leadershipCancel = cancel
	go func() {
		for evt := range ch {
			payload, ok := evt.Payload.(events.LeadershipChanged)
			if !ok {
				continue
			}
			if payload.Resource != cluster.ResourceKernel {
				continue
			}
			log.Printf("INFO: persistence observed control-plane role=%s", payload.Role)
		}
	}()
}

func (m *Module) Control() ControlStore {
	return m.control
}

func (m *Module) ControlVolume() VolumeHandle {
	return m.controlHandle
}

func (m *Module) Volumes() VolumeManager {
	return m.volumes
}

func (m *Module) Rootfs() RootfsVolumeManager {
	return m.rootfs
}

func (m *Module) Devices() DeviceManager {
	return m.devices
}

func (m *Module) StorageAdapter() StorageAdapter {
	return m.storage
}

func (m *Module) Consensus() ConsensusManager {
	return m.consensus
}

// ProvisionLUKSKeyslot adds or replaces a passphrase on the given LUKS keyslot
// across all v3 volumes. No-op if the volume manager is not LUKS-based.
func (m *Module) ProvisionLUKSKeyslot(ctx context.Context, slot int, passphrase []byte) error {
	lvm, ok := m.volumes.(*luksVolumeManager)
	if !ok {
		return nil
	}
	return lvm.provisionKeyslotOnAllVolumes(ctx, slot, passphrase)
}

func (m *Module) setLockState(ctx context.Context, locked bool) error {
	m.lockOpMu.Lock()
	defer m.lockOpMu.Unlock()

	store, ok := m.control.(lockableControlStore)
	if !ok {
		if locked {
			m.lockStateMu.Lock()
			m.lockState = true
			m.lockStateMu.Unlock()
			return nil
		}
		return ErrNotImplemented
	}
	if locked {
		store.Lock()
		// Lock teardown — must fail closed on ambiguity. Strict variant
		// returns an error rather than silently skipping for Foreign /
		// Unknown / Corrupted, so a "locked" return guarantees the
		// decrypted mapper is gone.
		if err := m.detachVolumeStrict(ctx, m.controlHandle); err != nil {
			return err
		}
		m.lockStateMu.Lock()
		m.lockState = true
		m.lockStateMu.Unlock()
		return nil
	}
	// Already unlocked — no-op under serialization.
	// Safe to read lockState without lockStateMu: lockOpMu serializes all writers,
	// and lockStateMu only exists for ControlLocked() readers on a separate path.
	if !m.lockState {
		return nil
	}
	if err := m.attachControlVolume(ctx); err != nil {
		return err
	}
	if err := store.Unlock(ctx); err != nil {
		store.Lock()
		// Best-effort rollback — the unlock failed; we're trying to
		// restore the locked state. If the rollback's own probe is
		// ambiguous we log and move on rather than wedging the caller.
		// CRITICAL surface: a rollback failure means the control store
		// was logically locked (Unlock errored) but its volume may still
		// be decrypted — operators must see this even though we
		// intentionally do not propagate (caller already has the
		// originating Unlock error and should see one root cause, not
		// two intermixed).
		if rollbackErr := m.detachVolumeBestEffort(ctx, m.controlHandle); rollbackErr != nil {
			log.Printf("CRITICAL: setLockState rollback after failed Unlock could not detach control volume: %v (original Unlock error: %v)", rollbackErr, err)
		}
		return err
	}
	m.lockStateMu.Lock()
	m.lockState = false
	m.lockStateMu.Unlock()
	return nil
}

// Shutdown terminates sub-components that require cleanup.
func (m *Module) Shutdown(ctx context.Context) error {
	// Stop workspace resize monitor before detaching volumes.
	if m.volumes != nil {
		if wrm, ok := m.volumes.(WorkspaceResizeMonitor); ok {
			wrm.StopWorkspaceResizeMonitor()
		}
	}

	m.commitMu.Lock()
	if m.pollCancel != nil {
		m.pollCancel()
		m.pollCancel = nil
	}
	m.commitMu.Unlock()
	m.healthMu.Lock()
	if m.healthCancel != nil {
		m.healthCancel()
		m.healthCancel = nil
	}
	m.healthMu.Unlock()
	m.commitMu.Lock()
	if m.leadershipCancel != nil {
		m.leadershipCancel()
		m.leadershipCancel = nil
	}
	m.commitMu.Unlock()
	if m.control != nil {
		_ = m.control.Close(ctx)
	}
	// Shutdown — best-effort. Probe ambiguity logs but doesn't block
	// shutdown; we're tearing down anyway.
	if err := m.detachVolumeBestEffort(ctx, m.controlHandle); err != nil {
		log.Printf("WARN: persistence failed to detach control volume: %v", err)
	}
	// Other sub-components expose explicit Stop methods when implemented.
	return nil
}

// detachClass is a typed classification of a probe outcome for
// detach-intent callers. Using a closed enum (rather than strings or raw
// AttachState) gives each wrapper a small, stable set of cases to dispatch
// on — easier to read and audit than a raw 7-state switch.
//
// IMPORTANT — Go does NOT have exhaustive-switch checking. Adding a new
// detachClass value will not be a compile error; the safety mechanism
// against "I forgot a case" is the runtime fail-closed `default` branch
// in each wrapper plus per-state tests, not the type system. Earlier
// drafts of this comment claimed compiler enforcement; codex iter-7
// correctly flagged that as an overstatement. Keep the default branches
// returning an error (strict) or logging+skipping (best-effort), and add
// a test row whenever a new detachClass case is introduced.
type detachClass int

const (
	// detachClassNoOp: invalid handle (no manager / empty ID / empty
	// MountDir). Caller treats as "nothing to do."
	detachClassNoOp detachClass = iota
	// detachClassProbeFailed: AttachStateOf returned an error, OR the
	// returned state is one we cannot safely act on (Unknown, Corrupted,
	// Foreign). Strict callers MUST propagate; best-effort logs and skips.
	// The accompanying error explains the cause for operators.
	detachClassProbeFailed
	// detachClassSkip: probe is conclusive that there is nothing of ours
	// to tear down (Detached). Caller no-ops without error.
	detachClassSkip
	// detachClassDetach: kernel state belongs to us (Attached, Partial,
	// Stale with our source token). Caller invokes Detach.
	detachClassDetach
)

// detachVolumeStrict tears down the volume's kernel state, failing closed
// on ANY ambiguity. Used by the lock-teardown path (setLockState→Lock):
// the contract there is "after this returns nil, the decrypted mapper is
// gone." A skip-on-ambiguity policy would let a "locked" system retain
// decrypted state — security regression.
//
// Implementation: switch on the typed detachClass returned by
// classifyForDetach. Go does not enforce switch exhaustiveness against
// the enum (see detachClass godoc) — the runtime safety net is the
// fail-closed default branch at the bottom that returns an error for any
// unhandled class, plus the test table covering each named case.
func (m *Module) detachVolumeStrict(ctx context.Context, handle VolumeHandle) error {
	class, err := m.classifyForDetach(ctx, handle)
	switch class {
	case detachClassNoOp, detachClassSkip:
		return nil
	case detachClassProbeFailed:
		return err
	case detachClassDetach:
		return m.volumes.Detach(ctx, handle)
	}
	// Unreachable for a well-formed detachClass value; keep an explicit
	// fail-closed default so any future-added case that bypasses the
	// switch refuses rather than silently succeeding.
	return fmt.Errorf("detachVolumeStrict: unhandled detachClass(%d) for %s", class, handle.ID)
}

// detachVolumeBestEffort is the same teardown intent but for non-security
// paths (Shutdown, lock rollback). The strict variant's "probe failure
// fails closed" policy doesn't fit shutdown — there's no caller waiting
// for a security guarantee, and refusing to make progress would block
// process exit on a transient probe blip. So this variant logs probe
// failures and continues. The exhaustive switch shape is the same; only
// the action for detachClassProbeFailed differs.
func (m *Module) detachVolumeBestEffort(ctx context.Context, handle VolumeHandle) error {
	class, err := m.classifyForDetach(ctx, handle)
	switch class {
	case detachClassNoOp, detachClassSkip:
		return nil
	case detachClassProbeFailed:
		log.Printf("WARN: detachVolumeBestEffort %s: %v", handle.ID, err)
		return nil
	case detachClassDetach:
		return m.volumes.Detach(ctx, handle)
	}
	log.Printf("WARN: detachVolumeBestEffort %s: unhandled detachClass(%d)", handle.ID, class)
	return nil
}

// classifyForDetach is the shared probe-and-classify shim. Folds every
// way the probe can say "I cannot tell you if this is yours" into the
// single ProbeFailed class — the strict wrapper then refuses to act and
// best-effort logs. Critically: state==Unknown / state==Corrupted are
// classified as ProbeFailed REGARDLESS of whether the probe attached an
// error. (Without that, the I/O-failure retry path inside
// probeAttachStateWithRetry — which returns (Unknown, nil) — would slip
// through as Skip, the same shape as the lock-leak regression that the
// typed wrappers were created to prevent.)
func (m *Module) classifyForDetach(ctx context.Context, handle VolumeHandle) (detachClass, error) {
	if m.volumes == nil || handle.ID == "" || handle.MountDir == "" {
		return detachClassNoOp, nil
	}
	state, err := m.volumes.AttachStateOf(ctx, handle.ID)
	if err != nil {
		return detachClassProbeFailed, fmt.Errorf("probe attach state for %s: %w", handle.ID, err)
	}
	switch state {
	case AttachStateAttached, AttachStatePartialMapperOnly, AttachStateStaleMountRecord:
		return detachClassDetach, nil
	case AttachStateDetached:
		return detachClassSkip, nil
	case AttachStateForeignMountAtPath:
		return detachClassProbeFailed, fmt.Errorf("foreign mount at %s for volume %s; refusing to detach (operator action required)", handle.MountDir, handle.ID)
	case AttachStateUnknown:
		return detachClassProbeFailed, fmt.Errorf("ambiguous kernel state (Unknown) for volume %s; refusing strict detach", handle.ID)
	case AttachStateKernelStateCorrupted:
		return detachClassProbeFailed, fmt.Errorf("kernel state corrupted for volume %s; refusing strict detach", handle.ID)
	}
	// AttachState exhaustiveness is enforced by the switch above; an
	// unrecognized value can only come from a future enum extension that
	// failed to update this site. Fail closed by classifying as ProbeFailed.
	return detachClassProbeFailed, fmt.Errorf("unrecognized AttachState(%d) for volume %s; refusing detach", int(state), handle.ID)
}
func (m *Module) publishLockState(locked bool) {
	if m.events == nil {
		return
	}
	m.events.Publish(events.Event{
		Topic: events.TopicLockStateChanged,
		Payload: events.LockStateChanged{
			Locked: locked,
		},
	})
}

// ControlLocked reports whether the control store is currently locked.
func (m *Module) ControlLocked() bool {
	m.lockStateMu.RLock()
	defer m.lockStateMu.RUnlock()
	return m.lockState
}

type revisionReporter interface {
	Revision(context.Context) (uint64, string, error)
}

type healthReporter interface {
	QuickCheck(context.Context) (ControlHealthReport, error)
}

const controlHealthTimeout = 5 * time.Second

func (m *Module) revisionSource() revisionReporter {
	if rep, ok := m.control.(revisionReporter); ok {
		return rep
	}
	return nil
}

func (m *Module) onControlCommit(ctx context.Context) {
	rep := m.revisionSource()
	if rep == nil {
		return
	}
	rev, checksum, err := rep.Revision(ctx)
	if err != nil {
		log.Printf("WARN: persistence commit revision read failed: %v", err)
		return
	}
	m.publishControlCommit(cluster.RoleLeader, rev, checksum)
}

func (m *Module) publishControlCommit(role cluster.Role, rev uint64, checksum string) {
	m.commitMu.Lock()
	if rev <= m.lastCommitRevision {
		m.commitMu.Unlock()
		return
	}
	m.lastCommitRevision = rev
	m.commitMu.Unlock()
	if m.events == nil {
		return
	}
	m.events.Publish(events.Event{
		Topic:   events.TopicControlStoreCommit,
		Payload: events.ControlStoreCommit{Revision: rev, Checksum: checksum, Role: role},
	})
}

func (m *Module) startRevisionPoller() {
	rep := m.revisionSource()
	if rep == nil {
		return
	}
	m.commitMu.Lock()
	if m.pollCancel != nil {
		m.commitMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.pollCancel = cancel
	m.commitMu.Unlock()
	go m.revisionPollLoop(ctx, rep)
}

func (m *Module) revisionPollLoop(ctx context.Context, rep revisionReporter) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.pollRevision(ctx, rep)
		}
	}
}

func (m *Module) pollRevision(ctx context.Context, rep revisionReporter) {
	if m.leadership != nil && m.leadership.Current(cluster.ResourceKernel) == cluster.RoleLeader {
		return
	}
	rev, checksum, err := rep.Revision(ctx)
	if err != nil {
		return
	}
	m.publishControlCommit(cluster.RoleFollower, rev, checksum)
}

func (m *Module) controlHealthSource() healthReporter {
	if rep, ok := m.control.(healthReporter); ok {
		return rep
	}
	return nil
}

func (m *Module) startControlHealthMonitor() {
	if m.events == nil {
		return
	}
	rep := m.controlHealthSource()
	if rep == nil {
		return
	}
	m.healthMu.Lock()
	if m.healthCancel != nil {
		m.healthMu.Unlock()
		return
	}
	interval := m.healthInterval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.healthCancel = cancel
	m.healthMu.Unlock()
	go m.controlHealthLoop(ctx, rep, interval)
}

func (m *Module) controlHealthLoop(ctx context.Context, rep healthReporter, interval time.Duration) {
	const maxRestarts = 3
	for attempt := 0; attempt <= maxRestarts; attempt++ {
		if attempt > 0 {
			// Check context before logging to avoid spurious restart warnings
			// during normal shutdown.
			select {
			case <-ctx.Done():
				return
			default:
			}
			log.Printf("WARN: control health monitor restarting (attempt %d/%d)", attempt, maxRestarts)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(attempt) * 30 * time.Second):
			}
		}
		m.runControlHealthLoop(ctx, rep, interval)
	}
	log.Printf("CRITICAL: control health monitor exhausted restarts, stopping permanently")
}

func (m *Module) runControlHealthLoop(ctx context.Context, rep healthReporter, interval time.Duration) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("CRITICAL: control health monitor panic: %v", r)
			if m.events != nil {
				m.events.Publish(events.Event{
					Topic: events.TopicControlHealth,
					Payload: ControlHealthReport{
						Status:    ControlHealthStatusError,
						Message:   fmt.Sprintf("health monitor crashed: %v", r),
						CheckedAt: time.Now().UTC(),
					},
				})
			}
		}
	}()
	m.runControlHealth(rep)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.runControlHealth(rep)
		}
	}
}

func (m *Module) runControlHealth(rep healthReporter) {
	if rep == nil || m.events == nil {
		return
	}
	checkCtx, cancel := context.WithTimeout(context.Background(), controlHealthTimeout)
	defer cancel()
	report, err := rep.QuickCheck(checkCtx)
	if err != nil {
		if report.Status != ControlHealthStatusError {
			report.Status = ControlHealthStatusError
		}
		if report.Message == "" {
			report.Message = err.Error()
		}
	}
	if report.CheckedAt.IsZero() {
		report.CheckedAt = time.Now().UTC()
	}
	m.events.Publish(events.Event{
		Topic:   events.TopicControlHealth,
		Payload: report,
	})
}
