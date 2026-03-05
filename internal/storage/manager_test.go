package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"piccolod/internal/events"
	"piccolod/internal/state/paths"
	"piccolod/internal/storage/lvm"
)

// exitError returns an *exec.ExitError with the given exit code.
func exitError(code int) *exec.ExitError {
	err := exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr
	}
	panic(fmt.Sprintf("expected *exec.ExitError for exit code %d, got %T", code, err))
}

// fakeDiskPreparer implements DiskPreparer for testing.
type fakeDiskPreparer struct {
	coreExists         bool
	partitionState     *PartitionState
	partitionStateErr  error
	createPartitionDev string
	createPartitionSlot int
	createPartitionErr error
	expandErr          error
	ensureDirsErr      error

	createCalls int
	expandCalls int
}

func (f *fakeDiskPreparer) VerifyPiccoloCoreExists(ctx context.Context, corePath string) bool {
	return f.coreExists
}

func (f *fakeDiskPreparer) GetPartitionState(ctx context.Context) (*PartitionState, error) {
	return f.partitionState, f.partitionStateErr
}

func (f *fakeDiskPreparer) CreateDataPartition(ctx context.Context, disk string) (string, int, error) {
	f.createCalls++
	return f.createPartitionDev, f.createPartitionSlot, f.createPartitionErr
}

func (f *fakeDiskPreparer) ExpandRootPartition(ctx context.Context, disk string, rootPartition string) error {
	f.expandCalls++
	return f.expandErr
}

func (f *fakeDiskPreparer) EnsureDirectories(ctx context.Context) error {
	return f.ensureDirsErr
}

func TestManager_Phase1_Success(t *testing.T) {
	core, _ := paths.SetRootsForTest(t)

	bus := events.NewBus()
	defer bus.Close()
	ch := bus.Subscribe(events.TopicStoragePhase1Complete, 1)

	prep := &fakeDiskPreparer{
		coreExists: true,
		partitionState: &PartitionState{
			Disk:          "/dev/sda",
			RootPartition: "/dev/sda2",
			DataPartition: "/dev/sda3",
		},
	}

	mgr := NewManager(prep, bus, nil)

	// Core dir must exist for VerifyPiccoloCoreExists.
	_ = core

	mgr.StartPartitioningAsync(context.Background())

	if err := mgr.WaitForPhase1(context.Background()); err != nil {
		t.Fatalf("expected success, got: %v", err)
	}

	if !mgr.IsPhase1Complete() {
		t.Error("expected phase 1 complete")
	}
	if mgr.IsEmergencyMode() {
		t.Errorf("unexpected emergency mode: %v", mgr.EmergencyError())
	}

	select {
	case evt := <-ch:
		payload := evt.Payload.(events.StoragePhase1Complete)
		if !payload.Success {
			t.Errorf("expected success event, got error: %s", payload.Error)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for phase 1 complete event")
	}
}

func TestManager_Phase1_MissingCore_HardEmergency(t *testing.T) {
	paths.SetRootsForTest(t)

	bus := events.NewBus()
	defer bus.Close()
	emergencyCh := bus.Subscribe(events.TopicStorageEmergency, 1)

	prep := &fakeDiskPreparer{
		coreExists: false, // Core missing → fail
		partitionState: &PartitionState{
			Disk: "/dev/sda",
		},
	}

	mgr := NewManager(prep, bus, nil)
	mgr.StartPartitioningAsync(context.Background())

	err := mgr.WaitForPhase1(context.Background())
	if err == nil {
		t.Fatal("expected error for missing core")
	}

	if mgr.GetEmergencyLevel() != EmergencyHard {
		t.Errorf("expected hard emergency, got %q", mgr.GetEmergencyLevel())
	}

	select {
	case evt := <-emergencyCh:
		payload := evt.Payload.(events.StorageEmergencyEvent)
		if payload.Level != "hard" {
			t.Errorf("expected hard level, got %q", payload.Level)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for emergency event")
	}
}

func TestManager_Phase1_FailWithOnboarding_SoftEmergency(t *testing.T) {
	core, _ := paths.SetRootsForTest(t)

	// Write onboarding.json with "complete" state.
	nbDir := filepath.Join(core, "network-bootstrap")
	if err := os.MkdirAll(nbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(onboardingState{State: "complete"})
	if err := os.WriteFile(filepath.Join(nbDir, "onboarding.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	bus := events.NewBus()
	defer bus.Close()

	prep := &fakeDiskPreparer{
		coreExists:        true,
		partitionStateErr: fmt.Errorf("simulated disk probe failure"),
	}

	mgr := NewManager(prep, bus, nil)
	mgr.StartPartitioningAsync(context.Background())

	err := mgr.WaitForPhase1(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}

	if mgr.GetEmergencyLevel() != EmergencySoft {
		t.Errorf("expected soft emergency, got %q", mgr.GetEmergencyLevel())
	}
}

func TestManager_Phase1_FailWithVG_SoftEmergency(t *testing.T) {
	paths.SetRootsForTest(t)

	bus := events.NewBus()
	defer bus.Close()

	// Phase 1 fails, but VG exists → soft emergency (previously set up).
	run := &fakeCommandRunner{} // vgs succeeds (no error configured)
	prep := &fakeDiskPreparerFunc{
		verifyCoreExists: func(ctx context.Context, p string) bool { return true },
		getPartitionState: func(ctx context.Context) (*PartitionState, error) {
			return nil, fmt.Errorf("simulated disk probe failure")
		},
	}

	mgr := NewManager(prep, bus, run)
	mgr.StartPartitioningAsync(context.Background())

	err := mgr.WaitForPhase1(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}

	if mgr.GetEmergencyLevel() != EmergencySoft {
		t.Errorf("expected soft emergency (VG signal), got %q", mgr.GetEmergencyLevel())
	}
}

func TestManager_Phase1_CreatePartition(t *testing.T) {
	paths.SetRootsForTest(t)

	bus := events.NewBus()
	defer bus.Close()

	prep := &fakeDiskPreparer{
		coreExists: true,
		partitionState: &PartitionState{
			Disk:          "/dev/sda",
			RootPartition: "/dev/sda2",
			// DataPartition empty — needs creation
		},
		createPartitionDev:  "/dev/sda3",
		createPartitionSlot: 3,
	}

	mgr := NewManager(prep, bus, nil)
	mgr.StartPartitioningAsync(context.Background())

	if err := mgr.WaitForPhase1(context.Background()); err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if prep.createCalls != 1 {
		t.Errorf("expected 1 create call, got %d", prep.createCalls)
	}
}

func TestManager_Phase1_ExpandRoot(t *testing.T) {
	paths.SetRootsForTest(t)

	bus := events.NewBus()
	defer bus.Close()

	prep := &fakeDiskPreparer{
		coreExists: true,
		partitionState: &PartitionState{
			Disk:              "/dev/sda",
			RootPartition:     "/dev/sda2",
			DataPartition:     "/dev/sda3",
			RootNeedsExpansion: true,
		},
	}

	mgr := NewManager(prep, bus, nil)
	mgr.StartPartitioningAsync(context.Background())

	if err := mgr.WaitForPhase1(context.Background()); err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if prep.expandCalls != 1 {
		t.Errorf("expected 1 expand call, got %d", prep.expandCalls)
	}
}

func TestManager_Phase1_RetryOnFailure(t *testing.T) {
	paths.SetRootsForTest(t)

	bus := events.NewBus()
	defer bus.Close()

	callCount := 0
	prep := &fakeDiskPreparerFunc{
		verifyCoreExists: func(ctx context.Context, p string) bool { return true },
		getPartitionState: func(ctx context.Context) (*PartitionState, error) {
			return &PartitionState{
				Disk:              "/dev/sda",
				RootPartition:     "/dev/sda2",
				RootNeedsExpansion: true,
				DataPartition:     "/dev/sda3",
			}, nil
		},
		expandRoot: func(ctx context.Context, disk string, root string) error {
			callCount++
			if callCount < 3 {
				return fmt.Errorf("transient failure %d", callCount)
			}
			return nil
		},
		ensureDirs: func(ctx context.Context) error { return nil },
	}

	mgr := NewManager(prep, bus, nil)
	// Use a background context — retries use phase1RetryBackoff (2s) but
	// we override nothing, so this test may take ~4 seconds.
	mgr.StartPartitioningAsync(context.Background())

	if err := mgr.WaitForPhase1(context.Background()); err != nil {
		t.Fatalf("expected success after retries, got: %v", err)
	}
	if callCount != 3 {
		t.Errorf("expected 3 expand calls (2 failures + 1 success), got %d", callCount)
	}
}

func TestManager_WaitForPhase1_ContextCancelled(t *testing.T) {
	paths.SetRootsForTest(t)

	bus := events.NewBus()
	defer bus.Close()

	prep := &fakeDiskPreparer{coreExists: true}
	mgr := NewManager(prep, bus, nil)
	// Don't start Phase 1 — wait should cancel.

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := mgr.WaitForPhase1(ctx)
	if err != context.DeadlineExceeded {
		t.Errorf("expected DeadlineExceeded, got: %v", err)
	}
}

func TestManager_IsPreviouslySetUp_NoSignals(t *testing.T) {
	paths.SetRootsForTest(t)

	bus := events.NewBus()
	defer bus.Close()

	prep := &fakeDiskPreparer{
		partitionState: &PartitionState{},
	}

	mgr := NewManager(prep, bus, nil)
	if mgr.IsPreviouslySetUp(context.Background()) {
		t.Error("expected false when no signals present")
	}
}

func TestManager_IsPreviouslySetUp_OnboardingComplete(t *testing.T) {
	core, _ := paths.SetRootsForTest(t)

	nbDir := filepath.Join(core, "network-bootstrap")
	if err := os.MkdirAll(nbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(onboardingState{State: "complete"})
	if err := os.WriteFile(filepath.Join(nbDir, "onboarding.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	bus := events.NewBus()
	defer bus.Close()

	prep := &fakeDiskPreparer{
		partitionState: &PartitionState{},
	}

	mgr := NewManager(prep, bus, nil)
	if !mgr.IsPreviouslySetUp(context.Background()) {
		t.Error("expected true when onboarding.json is complete")
	}
}

func TestManager_IsPreviouslySetUp_VGExists(t *testing.T) {
	paths.SetRootsForTest(t)

	bus := events.NewBus()
	defer bus.Close()

	// vgs succeeds → VG exists → previously set up.
	run := &fakeCommandRunner{}
	prep := &fakeDiskPreparer{
		partitionState: &PartitionState{},
	}

	mgr := NewManager(prep, bus, run)
	if !mgr.IsPreviouslySetUp(context.Background()) {
		t.Error("expected true when VG exists")
	}
}

func TestManager_InitializeDataVolume_NoDevice_ReturnsError(t *testing.T) {
	paths.SetRootsForTest(t)

	bus := events.NewBus()
	defer bus.Close()

	prep := &fakeDiskPreparer{coreExists: true}
	run := &fakeCommandRunner{}

	cfg := lvm.DefaultThinPoolConfig()
	mgr := &Manager{
		diskPrep:   prep,
		bus:        bus,
		run:        run,
		lvmPool:    lvm.NewPoolManager(run, bus, cfg),
		lvmVols:    lvm.NewLVManager(run, cfg.VGName, cfg.PoolName),
		phase1Done: make(chan struct{}),
	}
	// Simulate phase 1 complete with no data device.
	close(mgr.phase1Done)
	mgr.phase1Complete = true
	mgr.phase1Started = true

	err := mgr.InitializeDataVolume(context.Background())
	if err == nil {
		t.Fatal("expected error when device is empty")
	}
	if !strings.Contains(err.Error(), "no data partition discovered") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestManager_UnlockDataVolume_NoDevice_ReturnsError(t *testing.T) {
	paths.SetRootsForTest(t)

	bus := events.NewBus()
	defer bus.Close()

	prep := &fakeDiskPreparer{coreExists: true}
	run := &fakeCommandRunner{}

	cfg := lvm.DefaultThinPoolConfig()
	mgr := &Manager{
		diskPrep:   prep,
		bus:        bus,
		run:        run,
		lvmPool:    lvm.NewPoolManager(run, bus, cfg),
		lvmVols:    lvm.NewLVManager(run, cfg.VGName, cfg.PoolName),
		phase1Done: make(chan struct{}),
	}
	close(mgr.phase1Done)
	mgr.phase1Complete = true
	mgr.phase1Started = true

	err := mgr.UnlockDataVolume(context.Background())
	if err == nil {
		t.Fatal("expected error when device is empty")
	}
	if !strings.Contains(err.Error(), "no data partition discovered") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestManager_UnlockDataVolume_NoVG_FallsBackToInit(t *testing.T) {
	paths.SetRootsForTest(t)

	bus := events.NewBus()
	defer bus.Close()

	// fakeCommandRunner where vgs exits 5 (VG not found) and pvcreate fails
	// to confirm the fallback to InitializeDataVolume was entered.
	run := &fakeCommandRunner{
		errs: map[string]error{
			"vgs --noheadings piccolo-data-vg": exitError(5), // exit code 5 = VG not found
			"pvcreate -f /dev/sda3":            fmt.Errorf("simulated pvcreate failure"),
		},
	}

	cfg := lvm.DefaultThinPoolConfig()
	mgr := &Manager{
		diskPrep:   &fakeDiskPreparer{coreExists: true},
		bus:        bus,
		run:        run,
		lvmPool:    lvm.NewPoolManager(run, bus, cfg),
		lvmVols:    lvm.NewLVManager(run, cfg.VGName, cfg.PoolName),
		phase1Done: make(chan struct{}),
		dataDevice: "/dev/sda3",
	}
	close(mgr.phase1Done)
	mgr.phase1Complete = true
	mgr.phase1Started = true

	err := mgr.UnlockDataVolume(context.Background())
	// Should error from the init path (pvcreate fails).
	if err == nil {
		t.Fatal("expected error from fallback init path")
	}
	// Confirm it went through the VGExists check and then pvcreate.
	foundVgs := false
	foundPvcreate := false
	for _, call := range run.calls {
		if strings.Contains(call, "vgs") {
			foundVgs = true
		}
		if strings.Contains(call, "pvcreate") {
			foundPvcreate = true
		}
	}
	if !foundVgs {
		t.Error("expected vgs call for VGExists check")
	}
	if !foundPvcreate {
		t.Error("expected pvcreate call from fallback init path")
	}
}

func TestManager_UnlockDataVolume_TransientVGError_NoFallback(t *testing.T) {
	paths.SetRootsForTest(t)

	bus := events.NewBus()
	defer bus.Close()

	// fakeCommandRunner where vgs fails with a transient error (not exit code 5).
	// UnlockDataVolume must NOT fall back to destructive InitializeDataVolume.
	run := &fakeCommandRunner{
		errs: map[string]error{
			"vgs --noheadings piccolo-data-vg": fmt.Errorf("I/O error"),
		},
	}

	cfg := lvm.DefaultThinPoolConfig()
	mgr := &Manager{
		diskPrep:   &fakeDiskPreparer{coreExists: true},
		bus:        bus,
		run:        run,
		lvmPool:    lvm.NewPoolManager(run, bus, cfg),
		lvmVols:    lvm.NewLVManager(run, cfg.VGName, cfg.PoolName),
		phase1Done: make(chan struct{}),
		dataDevice: "/dev/sda3",
	}
	close(mgr.phase1Done)
	mgr.phase1Complete = true
	mgr.phase1Started = true

	err := mgr.UnlockDataVolume(context.Background())
	if err == nil {
		t.Fatal("expected error from transient VG check failure")
	}
	if !strings.Contains(err.Error(), "check VG existence") {
		t.Errorf("expected VG existence check error, got: %v", err)
	}
	// Verify pvcreate was NOT called (no destructive fallback).
	for _, call := range run.calls {
		if strings.Contains(call, "pvcreate") {
			t.Errorf("pvcreate should NOT be called on transient VG error, but got: %s", call)
		}
	}
}

func TestStartPartitioningAsync_Idempotent(t *testing.T) {
	paths.SetRootsForTest(t)

	bus := events.NewBus()
	defer bus.Close()

	startCount := 0
	prep := &fakeDiskPreparerFunc{
		verifyCoreExists: func(ctx context.Context, p string) bool { return true },
		getPartitionState: func(ctx context.Context) (*PartitionState, error) {
			startCount++
			return &PartitionState{
				Disk:          "/dev/sda",
				RootPartition: "/dev/sda2",
				DataPartition: "/dev/sda3",
			}, nil
		},
	}

	mgr := NewManager(prep, bus, nil)
	mgr.StartPartitioningAsync(context.Background())
	mgr.StartPartitioningAsync(context.Background()) // second call should be no-op

	if err := mgr.WaitForPhase1(context.Background()); err != nil {
		t.Fatalf("expected success, got: %v", err)
	}

	if startCount != 1 {
		t.Errorf("expected 1 Phase 1 run, got %d", startCount)
	}
}

func TestEnsurePhase1_StartsIfNotStarted(t *testing.T) {
	paths.SetRootsForTest(t)

	bus := events.NewBus()
	defer bus.Close()

	prep := &fakeDiskPreparer{
		coreExists: true,
		partitionState: &PartitionState{
			Disk:          "/dev/sda",
			RootPartition: "/dev/sda2",
			DataPartition: "/dev/sda3",
		},
	}

	mgr := NewManager(prep, bus, nil)
	// Don't call StartPartitioningAsync — EnsurePhase1 should start it.
	if err := mgr.EnsurePhase1(context.Background()); err != nil {
		t.Fatalf("EnsurePhase1() unexpected error: %v", err)
	}
	if !mgr.IsPhase1Complete() {
		t.Error("expected phase 1 complete after EnsurePhase1")
	}
}

// fakeCommandRunner implements runner.CommandRunner for storage manager tests.
type fakeCommandRunner struct {
	mu    sync.Mutex
	errs  map[string]error
	calls []string
}

func (f *fakeCommandRunner) Run(ctx context.Context, name string, args ...string) error {
	key := name
	for _, a := range args {
		key += " " + a
	}
	f.mu.Lock()
	f.calls = append(f.calls, key)
	f.mu.Unlock()
	if f.errs != nil {
		if err, ok := f.errs[key]; ok {
			return err
		}
	}
	return nil
}

func (f *fakeCommandRunner) RunWithOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	key := name
	for _, a := range args {
		key += " " + a
	}
	f.mu.Lock()
	f.calls = append(f.calls, key)
	f.mu.Unlock()
	if f.errs != nil {
		if err, ok := f.errs[key]; ok {
			return nil, err
		}
	}
	return nil, nil
}

func (f *fakeCommandRunner) RunWithStdin(ctx context.Context, stdin []byte, name string, args ...string) error {
	key := name
	for _, a := range args {
		key += " " + a
	}
	f.mu.Lock()
	f.calls = append(f.calls, key)
	f.mu.Unlock()
	if f.errs != nil {
		if err, ok := f.errs[key]; ok {
			return err
		}
	}
	return nil
}

// getCalls returns a copy of the calls slice (thread-safe).
func (f *fakeCommandRunner) getCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]string, len(f.calls))
	copy(cp, f.calls)
	return cp
}

// fakeDiskPreparerFunc allows per-method overrides for more flexible test scenarios.
type fakeDiskPreparerFunc struct {
	verifyCoreExists  func(ctx context.Context, corePath string) bool
	getPartitionState func(ctx context.Context) (*PartitionState, error)
	createPartition   func(ctx context.Context, disk string) (string, int, error)
	expandRoot        func(ctx context.Context, disk string, rootPartition string) error
	ensureDirs        func(ctx context.Context) error
}

func (f *fakeDiskPreparerFunc) VerifyPiccoloCoreExists(ctx context.Context, corePath string) bool {
	if f.verifyCoreExists != nil {
		return f.verifyCoreExists(ctx, corePath)
	}
	return true
}

func (f *fakeDiskPreparerFunc) GetPartitionState(ctx context.Context) (*PartitionState, error) {
	if f.getPartitionState != nil {
		return f.getPartitionState(ctx)
	}
	return &PartitionState{}, nil
}

func (f *fakeDiskPreparerFunc) CreateDataPartition(ctx context.Context, disk string) (string, int, error) {
	if f.createPartition != nil {
		return f.createPartition(ctx, disk)
	}
	return "/dev/sda3", 3, nil
}

func (f *fakeDiskPreparerFunc) ExpandRootPartition(ctx context.Context, disk string, rootPartition string) error {
	if f.expandRoot != nil {
		return f.expandRoot(ctx, disk, rootPartition)
	}
	return nil
}

func (f *fakeDiskPreparerFunc) EnsureDirectories(ctx context.Context) error {
	if f.ensureDirs != nil {
		return f.ensureDirs(ctx)
	}
	return nil
}
