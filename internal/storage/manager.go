package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"piccolod/internal/crypt"
	"piccolod/internal/events"
	"piccolod/internal/runner"
	"piccolod/internal/state/paths"
	"piccolod/internal/storage/luks"
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
)

// DiskPreparer abstracts disk probing and mutation operations.
// Implemented by diskprep.Preparer; decoupled here to avoid import cycles.
type DiskPreparer interface {
	VerifyPiccoloCoreExists(ctx context.Context, corePath string) bool
	GetPartitionState(ctx context.Context) (*PartitionState, error)
	CreateDataPartition(ctx context.Context, disk string) (partDev string, slot int, err error)
	ExpandRootPartition(ctx context.Context, disk string, rootPartition string) error
	EnsureDirectories(ctx context.Context) error
	SetNOCOWAttributes(ctx context.Context)
}

// Manager coordinates boot-time disk preparation and storage lifecycle.
type Manager struct {
	diskPrep DiskPreparer
	bus      *events.Bus
	run      runner.CommandRunner
	crypto   *crypt.Manager
	luksPool *luks.PoolManager

	mu             sync.RWMutex
	phase1Complete bool
	phase1Err      error
	emergency      EmergencyLevel
	emergencyErr   error
	dataDevice     string // discovered during Phase 1

	phase1Done   chan struct{}   // closed when Phase 1 finishes
	phase1Cancel context.CancelFunc // cancels in-flight Phase 1 on Stop
}

// NewManager creates a storage manager. The runner and crypto args are optional;
// when nil the LUKS pool manager is not initialized and LUKS facade methods
// become no-ops (suitable for dev mode or unit tests).
func NewManager(diskPrep DiskPreparer, bus *events.Bus, run runner.CommandRunner, crypto *crypt.Manager) *Manager {
	var pool *luks.PoolManager
	if run != nil && crypto != nil {
		pool = luks.NewPoolManager(run, crypto)
	}
	return &Manager{
		diskPrep:   diskPrep,
		bus:        bus,
		run:        run,
		crypto:     crypto,
		luksPool:   pool,
		phase1Done: make(chan struct{}),
	}
}

// Name implements supervisor.Component.
func (m *Manager) Name() string { return "storage-manager" }

// Start implements supervisor.Component. No-op; partitioning is triggered explicitly.
func (m *Manager) Start(ctx context.Context) error { return nil }

// Stop implements supervisor.Component. Cancels any in-flight Phase 1 operation.
func (m *Manager) Stop(ctx context.Context) error {
	m.mu.RLock()
	cancel := m.phase1Cancel
	m.mu.RUnlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

// StartPartitioningAsync launches Phase 1 disk preparation in the background.
// The goroutine is cancelled when Stop() is called.
func (m *Manager) StartPartitioningAsync(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	m.mu.Lock()
	m.phase1Cancel = cancel
	m.mu.Unlock()
	go m.runPhase1(ctx)
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

	// Directory creation and NOCOW attributes are deferred to Phase 2
	// (InitializeDataVolume / Unlock) because /piccolo-data is the LUKS mount
	// point and does not exist until the encrypted volume is opened.

	// Store data device for post-Phase-1 LUKS operations.
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
	if m.isPreviouslySetUp(ctx) {
		return EmergencySoft
	}
	return EmergencyHard
}

// onboardingState represents the relevant fields from onboarding.json.
type onboardingState struct {
	State string `json:"state"`
}

// isPreviouslySetUp uses a dual-signal check to determine if the device
// was previously set up. If EITHER signal is present, we treat the device
// as previously set up (soft emergency on failure, not hard).
func (m *Manager) isPreviouslySetUp(ctx context.Context) bool {
	// Signal 1: onboarding.json records explicit setup completion.
	onboardingPath := paths.CoreJoin("network-bootstrap", "onboarding.json")
	data, err := os.ReadFile(onboardingPath)
	if err == nil {
		var cfg onboardingState
		if json.Unmarshal(data, &cfg) == nil && cfg.State == "complete" {
			return true
		}
	}

	// Signal 2: LUKS header found on data partition.
	if m.diskPrep != nil {
		state, err := m.diskPrep.GetPartitionState(ctx)
		if err == nil && state.DataPartition != "" && state.DataPartitionLUKS {
			log.Printf("WARN: onboarding.json missing/incomplete, but LUKS header found; treating as previously set up")
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

// InitializeDataVolume formats LUKS on the data partition, unlocks it, mounts
// the data pool, and ensures the directory layout. Called from crypto setup.
func (m *Manager) InitializeDataVolume(ctx context.Context, adminPassword string, mnemonicKey []byte) error {
	if m.luksPool == nil {
		return nil
	}

	if err := m.WaitForPhase1(ctx); err != nil {
		return fmt.Errorf("wait for phase 1: %w", err)
	}

	device := m.DataDevice()
	if device == "" {
		return fmt.Errorf("no data partition discovered during phase 1")
	}

	// Check for orphaned LUKS header from a crashed previous init.
	if m.luksPool.DetectOrphanedLUKSHeader(ctx, device) {
		log.Printf("WARN: orphaned LUKS header detected; wiping before re-init")
		if err := m.luksPool.WipeLUKSHeader(ctx, device); err != nil {
			return fmt.Errorf("wipe orphaned LUKS header: %w", err)
		}
	}

	if err := m.luksPool.InitializeLUKS(ctx, device, adminPassword, mnemonicKey); err != nil {
		return fmt.Errorf("initialize LUKS: %w", err)
	}

	if err := m.luksPool.Unlock(ctx, device, adminPassword); err != nil {
		return fmt.Errorf("unlock after init: %w", err)
	}

	// Create btrfs filesystem on the freshly initialized LUKS container.
	mapperPath := luks.MapperPath(0)
	if err := m.run.Run(ctx, "mkfs.btrfs", "-L", "piccolo-data", mapperPath); err != nil {
		return fmt.Errorf("mkfs.btrfs on data pool: %w", err)
	}

	if err := m.luksPool.MountDataPool(ctx); err != nil {
		return fmt.Errorf("mount data pool: %w", err)
	}

	// Create directory layout on the freshly mounted data pool.
	if m.diskPrep != nil {
		if err := m.diskPrep.EnsureDirectories(ctx); err != nil {
			log.Printf("WARN: ensure directories after LUKS init: %v", err)
		}
		m.diskPrep.SetNOCOWAttributes(ctx)
	}

	if m.bus != nil {
		m.bus.Publish(events.Event{Topic: events.TopicStorageLUKSInitialized})
	}
	log.Printf("data volume initialized and mounted")
	return nil
}

// UnlockDataVolume unlocks the LUKS data partition, mounts it, ensures
// directories, and resumes any interrupted keyslot rotations.
func (m *Manager) UnlockDataVolume(ctx context.Context, adminPassword string) error {
	if m.luksPool == nil {
		return nil
	}

	// Block until Phase 1 completes — dataDevice is set at end of Phase 1.
	if err := m.WaitForPhase1(ctx); err != nil {
		return fmt.Errorf("wait for phase 1: %w", err)
	}

	device := m.DataDevice()
	if device == "" {
		return fmt.Errorf("no data partition discovered during phase 1")
	}

	// Recovery: if no LUKS header, previous setup's init failed before format.
	// Fall back to full initialization (Posture RFC §5.3).
	// nil mnemonicKey: slot 2 is enrolled separately when recovery key is generated.
	hasHeader, headerErr := m.luksPool.HasLUKSHeader(ctx, device)
	if headerErr != nil {
		return fmt.Errorf("check LUKS header on %s: %w", device, headerErr)
	}
	if !hasHeader {
		log.Printf("WARN: no LUKS header on %s — falling back to initialization", device)
		return m.InitializeDataVolume(ctx, adminPassword, nil)
	}

	if err := m.luksPool.Unlock(ctx, device, adminPassword); err != nil {
		return fmt.Errorf("unlock LUKS: %w", err)
	}

	if err := m.luksPool.MountDataPool(ctx); err != nil {
		return fmt.Errorf("mount data pool: %w", err)
	}

	// Post-unlock housekeeping.
	if m.diskPrep != nil {
		if err := m.diskPrep.EnsureDirectories(ctx); err != nil {
			log.Printf("WARN: ensure directories after unlock: %v", err)
		}
		m.diskPrep.SetNOCOWAttributes(ctx)
	}

	// Resume interrupted keyslot rotations.
	if err := m.luksPool.ResumePasswordRotationIfNeeded(ctx, device, adminPassword); err != nil {
		log.Printf("WARN: resume password rotation: %v", err)
	}
	// Mnemonic rotation deferred until key is available (nil → logs and returns).
	if err := m.luksPool.ResumeMnemonicRotationIfNeeded(ctx, device, nil); err != nil {
		log.Printf("WARN: resume mnemonic rotation: %v", err)
	}

	if m.bus != nil {
		m.bus.Publish(events.Event{Topic: events.TopicStorageLUKSUnlocked})
	}
	log.Printf("data volume unlocked and mounted")
	return nil
}

// LockDataVolume unmounts and closes the LUKS data partition.
func (m *Manager) LockDataVolume(ctx context.Context) error {
	if m.luksPool == nil {
		return nil
	}

	if err := m.luksPool.Lock(ctx); err != nil {
		return fmt.Errorf("lock data pool: %w", err)
	}

	if m.bus != nil {
		m.bus.Publish(events.Event{Topic: events.TopicStorageLocked})
	}
	log.Printf("data volume locked")
	return nil
}

// OnAdminPasswordRotated rotates the admin password keyslot on the data partition.
func (m *Manager) OnAdminPasswordRotated(ctx context.Context, oldPass, newPass string) error {
	if m.luksPool == nil {
		return nil
	}
	device := m.DataDevice()
	if device == "" {
		return nil
	}
	return m.luksPool.OnAdminPasswordRotated(ctx, device, oldPass, newPass)
}

// OnRecoveryMnemonicRotated rotates the mnemonic keyslot on the data partition.
func (m *Manager) OnRecoveryMnemonicRotated(ctx context.Context, oldKey, newKey []byte) error {
	if m.luksPool == nil {
		return nil
	}
	device := m.DataDevice()
	if device == "" {
		return nil
	}
	return m.luksPool.OnRecoveryMnemonicRotated(ctx, device, oldKey, newKey)
}
