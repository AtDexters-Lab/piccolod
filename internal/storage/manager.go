package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"piccolod/internal/events"
	"piccolod/internal/resources/pressure"
	"piccolod/internal/runner"
	"piccolod/internal/state/paths"
	"piccolod/internal/storage/lvm"
)

// EmergencyLevel classifies storage failure severity.
type EmergencyLevel string

const (
	EmergencyNone EmergencyLevel = ""
	EmergencyHard EmergencyLevel = "hard"
	EmergencySoft EmergencyLevel = "soft"

	phase1MaxRetries   = 3
	phase1RetryBackoff = 2 * time.Second
	phase1Timeout      = 5 * time.Minute

	// healthMonitorInterval is the interval for thin pool health checks.
	healthMonitorInterval = 60 * time.Second
)

// DiskPreparer abstracts disk probing and mutation operations.
// Implemented by diskprep.Preparer; decoupled here to avoid import cycles.
type DiskPreparer interface {
	VerifyPiccoloCoreExists(ctx context.Context, corePath string) bool
	GetPartitionState(ctx context.Context) (*PartitionState, error)
	CreateDataPartition(ctx context.Context, disk string) (partDev string, slot int, err error)
	ExpandRootPartition(ctx context.Context, disk string, rootPartition string) error
	ExpandDataPartition(ctx context.Context, disk string, dataPartition string) error
	EnsureDirectories(ctx context.Context) error
}

// Manager coordinates boot-time disk preparation and storage lifecycle.
// In the block-native architecture, the data partition hosts an LVM thin pool
// directly (no pool-level LUKS). Per-volume encryption is handled in M3.
type Manager struct {
	diskPrep DiskPreparer
	bus      *events.Bus
	run      runner.CommandRunner
	lvmPool  *lvm.PoolManager
	lvmVols  *lvm.LVManager

	mu             sync.RWMutex
	phase1Complete bool
	phase1Err      error
	phase1Started  bool // set to true on first StartPartitioningAsync call
	emergency      EmergencyLevel
	emergencyErr   error
	dataDevice     string // discovered during Phase 1

	phase1Done   chan struct{}      // closed when Phase 1 finishes
	phase1Cancel context.CancelFunc // cancels in-flight Phase 1 on Stop
}

// NewManager creates a storage manager.
func NewManager(diskPrep DiskPreparer, bus *events.Bus, run runner.CommandRunner) *Manager {
	var pool *lvm.PoolManager
	var vols *lvm.LVManager
	if run != nil {
		cfg := lvm.DefaultThinPoolConfig()
		pool = lvm.NewPoolManager(run, bus, cfg)
		vols = lvm.NewLVManager(run, cfg.VGName, cfg.PoolName)
	}
	return &Manager{
		diskPrep:   diskPrep,
		bus:        bus,
		run:        run,
		lvmPool:    pool,
		lvmVols:    vols,
		phase1Done: make(chan struct{}),
	}
}

// LVMVolumes returns the LV manager for creating per-volume thin LVs.
func (m *Manager) LVMVolumes() *lvm.LVManager {
	return m.lvmVols
}

// LVMPool returns the thin pool manager for capacity checks.
func (m *Manager) LVMPool() *lvm.PoolManager {
	return m.lvmPool
}

// Name implements supervisor.Component.
func (m *Manager) Name() string { return "storage-manager" }

// Start implements supervisor.Component. No-op; partitioning is triggered explicitly.
func (m *Manager) Start(ctx context.Context) error { return nil }

// Stop implements supervisor.Component. Cancels any in-flight Phase 1 operation
// and stops the thin pool health monitor.
func (m *Manager) Stop(ctx context.Context) error {
	m.mu.RLock()
	cancel := m.phase1Cancel
	m.mu.RUnlock()
	if cancel != nil {
		cancel()
	}
	if m.lvmPool != nil {
		m.lvmPool.StopHealthMonitor()
	}
	return nil
}

// StartPartitioningAsync launches Phase 1 disk preparation in the background.
// Idempotent: subsequent calls after the first are no-ops, preventing the
// stale-cancel race where a second call would overwrite phase1Cancel and
// orphan the first goroutine. The goroutine is cancelled when Stop() is called.
func (m *Manager) StartPartitioningAsync(ctx context.Context) {
	if err := m.startPartitioning(ctx); err != nil {
		log.Printf("INFO: storage phase 1 deferred: %v", err)
	}
}

func (m *Manager) startPartitioning(ctx context.Context) error {
	ctx = pressure.WithWorkClass(ctx, pressure.WorkStorage)
	if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkStorage); err != nil {
		return err
	}
	m.mu.Lock()
	if m.phase1Started {
		m.mu.Unlock()
		return nil
	}
	m.phase1Started = true
	ctx = pressure.WithTransitionContinuation(ctx)
	ctx, cancel := context.WithCancel(ctx)
	m.phase1Cancel = cancel
	m.mu.Unlock()
	go m.runPhase1(ctx)
	return nil
}

// EnsurePhase1 starts Phase 1 if not already started and waits for completion.
// Callers use this instead of raw WaitForPhase1 to handle the case where
// Phase 1 hasn't been started yet (e.g. install_disk path skips try_piccolo).
func (m *Manager) EnsurePhase1(ctx context.Context) error {
	ctx = pressure.WithWorkClass(ctx, pressure.WorkStorage)
	if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkStorage); err != nil {
		return err
	}
	// Detach the long-running phase from the request lifetime without dropping
	// durable-transition admission carried in context values.
	if err := m.startPartitioning(context.WithoutCancel(ctx)); err != nil {
		return err
	}
	return m.WaitForPhase1(ctx)
}

// WaitForPhase1 blocks until Phase 1 completes or ctx is cancelled.
func (m *Manager) WaitForPhase1(ctx context.Context) error {
	select {
	case <-m.phase1Done:
		m.mu.RLock()
		defer m.mu.RUnlock()
		return m.phase1Err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// IsPhase1Complete returns whether Phase 1 has finished.
func (m *Manager) IsPhase1Complete() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.phase1Complete
}

// IsPhase1Started reports whether the post-Ready recovery pass actually
// launched storage preparation. USB onboarding may intentionally defer it.
func (m *Manager) IsPhase1Started() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.phase1Started
}

// IsEmergencyMode returns whether storage is in emergency mode.
func (m *Manager) IsEmergencyMode() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.emergency != EmergencyNone
}

// GetEmergencyLevel returns the current emergency level.
func (m *Manager) GetEmergencyLevel() EmergencyLevel {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.emergency
}

// EmergencyError returns the error that caused emergency mode.
func (m *Manager) EmergencyError() error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.emergencyErr
}

// runPhase1 executes the Phase 1 disk preparation sequence.
func (m *Manager) runPhase1(parentCtx context.Context) {
	releaseOwner := pressure.BeginLifecycleOwner("storage")
	defer releaseOwner()
	ctx, cancel := context.WithTimeout(parentCtx, phase1Timeout)
	defer cancel()

	defer close(m.phase1Done)

	err := m.preparePartitioning(ctx)

	m.mu.Lock()
	m.phase1Complete = true
	m.phase1Err = err
	m.mu.Unlock()

	if err != nil {
		level := m.classifyFailure(ctx)
		m.mu.Lock()
		m.emergency = level
		m.emergencyErr = err
		m.mu.Unlock()

		log.Printf("ERROR: storage phase 1 failed (%s emergency): %v", level, err)
		m.bus.Publish(events.Event{
			Topic: events.TopicStorageEmergency,
			Payload: events.StorageEmergencyEvent{
				Level: string(level),
				Error: err.Error(),
			},
		})
		m.bus.Publish(events.Event{
			Topic: events.TopicStoragePhase1Complete,
			Payload: events.StoragePhase1Complete{
				Success: false,
				Error:   err.Error(),
			},
		})
		return
	}

	log.Printf("storage phase 1 complete")
	m.bus.Publish(events.Event{
		Topic:   events.TopicStoragePhase1Complete,
		Payload: events.StoragePhase1Complete{Success: true},
	})
}

// preparePartitioning runs the Phase 1 operations in sequence.
func (m *Manager) preparePartitioning(ctx context.Context) error {
	// Step 1: Verify core subvolume exists (deterministic, no retry).
	if !m.diskPrep.VerifyPiccoloCoreExists(ctx, paths.CoreRoot()) {
		return fmt.Errorf("core subvolume missing at %s", paths.CoreRoot())
	}

	// Step 2: Survey current disk state.
	state, err := m.diskPrep.GetPartitionState(ctx)
	if err != nil {
		return fmt.Errorf("survey disk: %w", err)
	}

	// Step 3: Create data partition if absent (with retries).
	if state.DataPartition == "" {
		err := m.retryPhase1Op(ctx, "create-data-partition", func(ctx context.Context) error {
			partDev, slot, err := m.diskPrep.CreateDataPartition(ctx, state.Disk)
			if err != nil {
				return err
			}
			state.DataPartition = partDev
			state.DataPartitionSlot = slot
			return nil
		})
		if err != nil {
			return err
		}
	}

	// Step 4: Expand root partition if needed (with retries).
	if state.RootNeedsExpansion {
		err := m.retryPhase1Op(ctx, "expand-root-partition", func(ctx context.Context) error {
			return m.diskPrep.ExpandRootPartition(ctx, state.Disk, state.RootPartition)
		})
		if err != nil {
			return err
		}
	}

	// Step 5: Expand data partition if unallocated space follows it.
	// Must run AFTER root expansion — root expansion uses the data partition as
	// a growpart boundary, so the data partition must not move until root is done.
	// Non-fatal: the existing partition and VG are usable at their old size.
	// Phase 2 ExpandPool will pick up whatever partition size is available.
	if state.DataNeedsExpansion {
		if err := m.diskPrep.ExpandDataPartition(ctx, state.Disk, state.DataPartition); err != nil {
			log.Printf("WARN: data partition expansion failed (non-fatal): %v", err)
		}
	}

	// Store data device for post-Phase-1 LVM operations.
	m.mu.Lock()
	m.dataDevice = state.DataPartition
	m.mu.Unlock()

	return nil
}

// retryPhase1Op retries an operation up to phase1MaxRetries times with backoff.
func (m *Manager) retryPhase1Op(ctx context.Context, name string, op func(context.Context) error) error {
	var lastErr error
	for attempt := 1; attempt <= phase1MaxRetries; attempt++ {
		lastErr = op(ctx)
		if lastErr == nil {
			return nil
		}
		if attempt < phase1MaxRetries {
			log.Printf("WARN: phase 1 operation %q failed (attempt %d/%d): %v",
				name, attempt, phase1MaxRetries, lastErr)
			select {
			case <-time.After(phase1RetryBackoff):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return fmt.Errorf("%s failed after %d attempts: %w", name, phase1MaxRetries, lastErr)
}

// classifyFailure determines whether a Phase 1 failure should be hard or soft emergency.
func (m *Manager) classifyFailure(ctx context.Context) EmergencyLevel {
	if m.IsPreviouslySetUp(ctx) {
		return EmergencySoft
	}
	return EmergencyHard
}

// onboardingState represents the relevant fields from onboarding.json.
type onboardingState struct {
	State string `json:"state"`
}

// IsPreviouslySetUp uses a dual-signal check to determine if the device
// was previously set up. If EITHER signal is present, we treat the device
// as previously set up (soft emergency on failure, not hard).
func (m *Manager) IsPreviouslySetUp(ctx context.Context) bool {
	ctx = pressure.WithWorkClass(ctx, pressure.WorkStorage)
	// Signal 1: onboarding.json records explicit setup completion.
	onboardingPath := paths.CoreJoin("network-bootstrap", "onboarding.json")
	data, err := os.ReadFile(onboardingPath)
	if err == nil {
		var cfg onboardingState
		if json.Unmarshal(data, &cfg) == nil && cfg.State == "complete" {
			return true
		}
	}

	// Signal 2: LVM VG exists on the data partition.
	if m.lvmPool != nil {
		exists, err := m.lvmPool.VGExists(ctx)
		if err != nil {
			log.Printf("WARN: VGExists check failed: %v; treating as not previously set up", err)
		} else if exists {
			log.Printf("WARN: onboarding.json missing/incomplete, but LVM VG found; treating as previously set up")
			return true
		}
	}

	return false
}

// DataDevice returns the data partition device path discovered during Phase 1.
func (m *Manager) DataDevice() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.dataDevice
}

// InitializeDataVolume creates the LVM VG and thin pool on the raw data
// partition. No pool-level LUKS — per-volume encryption is handled by M3.
// Also creates the ephemeral podman shared thin LV with btrfs+zstd.
func (m *Manager) InitializeDataVolume(ctx context.Context) error {
	ctx = pressure.WithWorkClass(ctx, pressure.WorkStorage)
	if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkStorage); err != nil {
		return err
	}
	if m.lvmPool == nil {
		return nil
	}

	if err := m.EnsurePhase1(ctx); err != nil {
		return fmt.Errorf("wait for phase 1: %w", err)
	}

	device := m.DataDevice()
	if device == "" {
		return fmt.Errorf("no data partition discovered during phase 1")
	}

	// Create LVM VG + thin pool on the raw partition.
	if err := m.lvmPool.CreatePool(ctx, device); err != nil {
		return fmt.Errorf("create LVM pool: %w", err)
	}

	// Ensure directory layout on the core root.
	if m.diskPrep != nil {
		if err := m.diskPrep.EnsureDirectories(ctx); err != nil {
			log.Printf("WARN: ensure directories after LVM init: %v", err)
		}
	}

	// Start thin pool health monitoring.
	m.lvmPool.StartHealthMonitor(healthMonitorInterval)

	if m.bus != nil {
		m.bus.Publish(events.Event{Topic: events.TopicStoragePoolInitialized})
	}
	log.Printf("storage: LVM thin pool initialized on %s", device)
	return nil
}

// UnlockDataVolume activates the LVM VG and thin pool. No password needed —
// there is no pool-level LUKS. Per-volume LUKS handled in M3.
func (m *Manager) UnlockDataVolume(ctx context.Context) error {
	ctx = pressure.WithWorkClass(ctx, pressure.WorkStorage)
	if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkStorage); err != nil {
		return err
	}
	if m.lvmPool == nil {
		return nil
	}

	if err := m.EnsurePhase1(ctx); err != nil {
		return fmt.Errorf("wait for phase 1: %w", err)
	}

	device := m.DataDevice()
	if device == "" {
		return fmt.Errorf("no data partition discovered during phase 1")
	}

	// Check if the VG exists. If not, fall back to initialization.
	// Distinguish "VG not found" from transient errors — only fall back
	// to init on a confirmed absence to avoid destructive pvcreate -f.
	vgExists, vgErr := m.lvmPool.VGExists(ctx)
	if vgErr != nil {
		return fmt.Errorf("check VG existence: %w", vgErr)
	}
	if !vgExists {
		log.Printf("WARN: no LVM VG found — falling back to initialization")
		return m.InitializeDataVolume(ctx)
	}

	// Activate the VG (no password — no pool-level LUKS).
	if err := m.lvmPool.ActivatePool(ctx); err != nil {
		return fmt.Errorf("activate LVM pool: %w", err)
	}

	// Expand PV and thin pool if the data partition grew (Phase 1 growpart).
	// Must run before StartHealthMonitor to avoid false threshold alerts from
	// the pool's pre-expansion size. Non-fatal: pool works at old size.
	if err := m.lvmPool.ExpandPool(ctx, device); err != nil {
		log.Printf("WARN: thin pool expansion failed (non-fatal): %v", err)
	}

	// Ensure directory layout.
	if m.diskPrep != nil {
		if err := m.diskPrep.EnsureDirectories(ctx); err != nil {
			log.Printf("WARN: ensure directories after VG activate: %v", err)
		}
	}

	// Start thin pool health monitoring.
	m.lvmPool.StartHealthMonitor(healthMonitorInterval)

	if m.bus != nil {
		m.bus.Publish(events.Event{Topic: events.TopicStoragePoolActivated})
	}
	log.Printf("storage: LVM VG activated")
	return nil
}

// LockDataVolume deactivates the LVM VG.
func (m *Manager) LockDataVolume(ctx context.Context) error {
	ctx = pressure.WithWorkClass(ctx, pressure.WorkStorage)
	if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkStorage); err != nil {
		return err
	}
	if m.lvmPool == nil {
		return nil
	}

	m.lvmPool.StopHealthMonitor()

	// Deactivate the VG.
	if err := m.lvmPool.DeactivatePool(ctx); err != nil {
		return fmt.Errorf("deactivate LVM pool: %w", err)
	}

	if m.bus != nil {
		m.bus.Publish(events.Event{Topic: events.TopicStorageLocked})
	}
	log.Printf("storage: LVM VG deactivated")
	return nil
}
