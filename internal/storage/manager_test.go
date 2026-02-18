package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"piccolod/internal/events"
	"piccolod/internal/state/paths"
	"piccolod/internal/storage/luks"
)

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

func (f *fakeDiskPreparer) SetNOCOWAttributes(ctx context.Context) {}

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

	mgr := NewManager(prep, bus, nil, nil)

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

	mgr := NewManager(prep, bus, nil, nil)
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

	mgr := NewManager(prep, bus, nil, nil)
	mgr.StartPartitioningAsync(context.Background())

	err := mgr.WaitForPhase1(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}

	if mgr.GetEmergencyLevel() != EmergencySoft {
		t.Errorf("expected soft emergency, got %q", mgr.GetEmergencyLevel())
	}
}

func TestManager_Phase1_FailWithLUKS_SoftEmergency(t *testing.T) {
	paths.SetRootsForTest(t)

	bus := events.NewBus()
	defer bus.Close()

	// First GetPartitionState call (from preparePartitioning) fails.
	// Second GetPartitionState call (from isPreviouslySetUp) returns LUKS data.
	callCount := 0
	prep := &fakeDiskPreparerFunc{
		verifyCoreExists: func(ctx context.Context, p string) bool { return true },
		getPartitionState: func(ctx context.Context) (*PartitionState, error) {
			callCount++
			if callCount == 1 {
				return nil, fmt.Errorf("simulated disk probe failure")
			}
			// Second call (from isPreviouslySetUp): return state with LUKS.
			return &PartitionState{
				DataPartition:     "/dev/sda3",
				DataPartitionLUKS: true,
			}, nil
		},
	}

	mgr := NewManager(prep, bus, nil, nil)
	mgr.StartPartitioningAsync(context.Background())

	err := mgr.WaitForPhase1(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}

	if mgr.GetEmergencyLevel() != EmergencySoft {
		t.Errorf("expected soft emergency (LUKS signal), got %q", mgr.GetEmergencyLevel())
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

	mgr := NewManager(prep, bus, nil, nil)
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

	mgr := NewManager(prep, bus, nil, nil)
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
		setNOCOW:   func(ctx context.Context) {},
	}

	mgr := NewManager(prep, bus, nil, nil)
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
	mgr := NewManager(prep, bus, nil, nil)
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

	mgr := NewManager(prep, bus, nil, nil)
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

	mgr := NewManager(prep, bus, nil, nil)
	if !mgr.IsPreviouslySetUp(context.Background()) {
		t.Error("expected true when onboarding.json is complete")
	}
}

func TestManager_IsPreviouslySetUp_LUKSHeader(t *testing.T) {
	paths.SetRootsForTest(t)

	bus := events.NewBus()
	defer bus.Close()

	prep := &fakeDiskPreparer{
		partitionState: &PartitionState{
			DataPartition:     "/dev/sda3",
			DataPartitionLUKS: true,
		},
	}

	mgr := NewManager(prep, bus, nil, nil)
	if !mgr.IsPreviouslySetUp(context.Background()) {
		t.Error("expected true when LUKS header found")
	}
}

func TestManager_InitializeDataVolume_NoDevice_ReturnsError(t *testing.T) {
	paths.SetRootsForTest(t)

	bus := events.NewBus()
	defer bus.Close()

	prep := &fakeDiskPreparer{coreExists: true}
	run := &fakeCommandRunner{}

	mgr := &Manager{
		diskPrep:   prep,
		bus:        bus,
		run:        run,
		luksPool:   luks.NewPoolManager(run, nil),
		phase1Done: make(chan struct{}),
	}
	// Simulate phase 1 complete with no data device.
	close(mgr.phase1Done)
	mgr.phase1Complete = true
	mgr.phase1Started = true

	err := mgr.InitializeDataVolume(context.Background(), "password", nil)
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

	mgr := &Manager{
		diskPrep:   prep,
		bus:        bus,
		run:        run,
		luksPool:   luks.NewPoolManager(run, nil),
		phase1Done: make(chan struct{}),
	}
	close(mgr.phase1Done)
	mgr.phase1Complete = true
	mgr.phase1Started = true

	err := mgr.UnlockDataVolume(context.Background(), "password")
	if err == nil {
		t.Fatal("expected error when device is empty")
	}
	if !strings.Contains(err.Error(), "no data partition discovered") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestManager_UnlockDataVolume_NoLUKSHeader_FallsBackToInit(t *testing.T) {
	paths.SetRootsForTest(t)

	bus := events.NewBus()
	defer bus.Close()

	// fakeCommandRunner that fails isLuks (no header) and fails luksFormat
	// to confirm that the fallback path was entered.
	run := &fakeCommandRunner{
		errs: map[string]error{
			"cryptsetup isLuks /dev/sda3": exitCode1(),
			// InitializeDataVolume will try luksFormat which will fail —
			// that's fine, we just need to confirm the fallback was invoked.
			"cryptsetup luksFormat --type luks2 --batch-mode --label piccolo-data --cipher aes-xts-plain64 --key-size 512 --hash sha256 --pbkdf pbkdf2 --pbkdf-force-iterations 1000 --key-file /run/piccolo/piccolo_data_pool_key /dev/sda3": fmt.Errorf("simulated"),
		},
	}

	mgr := &Manager{
		diskPrep:   &fakeDiskPreparer{coreExists: true},
		bus:        bus,
		run:        run,
		luksPool:   luks.NewPoolManager(run, nil),
		phase1Done: make(chan struct{}),
		dataDevice: "/dev/sda3",
	}
	close(mgr.phase1Done)
	mgr.phase1Complete = true
	mgr.phase1Started = true

	err := mgr.UnlockDataVolume(context.Background(), "password")
	// Should error from the init path (generate pool keyfile fails or luksFormat fails),
	// not from unlock.
	if err == nil {
		t.Fatal("expected error from fallback init path")
	}
	// Confirm it went through the HasLUKSHeader check (isLuks call).
	found := false
	for _, call := range run.calls {
		if strings.Contains(call, "isLuks") {
			found = true
		}
	}
	if !found {
		t.Error("expected HasLUKSHeader (isLuks) call in fallback path")
	}
}

// exitCode1 returns an *exec.ExitError with exit code 1.
func exitCode1() *exec.ExitError {
	err := exec.Command("sh", "-c", "exit 1").Run()
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr
	}
	panic("expected *exec.ExitError")
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

	mgr := NewManager(prep, bus, nil, nil)
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

	mgr := NewManager(prep, bus, nil, nil)
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
	errs  map[string]error
	calls []string
}

func (f *fakeCommandRunner) Run(ctx context.Context, name string, args ...string) error {
	key := name
	for _, a := range args {
		key += " " + a
	}
	f.calls = append(f.calls, key)
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
	f.calls = append(f.calls, key)
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
	f.calls = append(f.calls, key)
	if f.errs != nil {
		if err, ok := f.errs[key]; ok {
			return err
		}
	}
	return nil
}

// fakeDiskPreparerFunc allows per-method overrides for more flexible test scenarios.
type fakeDiskPreparerFunc struct {
	verifyCoreExists  func(ctx context.Context, corePath string) bool
	getPartitionState func(ctx context.Context) (*PartitionState, error)
	createPartition   func(ctx context.Context, disk string) (string, int, error)
	expandRoot        func(ctx context.Context, disk string, rootPartition string) error
	ensureDirs        func(ctx context.Context) error
	setNOCOW          func(ctx context.Context)
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

func (f *fakeDiskPreparerFunc) SetNOCOWAttributes(ctx context.Context) {
	if f.setNOCOW != nil {
		f.setNOCOW(ctx)
	}
}
