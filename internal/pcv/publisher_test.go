package pcv

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"piccolod/internal/events"
	"piccolod/internal/resources/pressure"
	"piccolod/internal/state/paths"
)

// stubRunner records commands without executing them.
type stubRunner struct {
	commands [][]string
}

func (r *stubRunner) Run(ctx context.Context, name string, args ...string) error {
	r.commands = append(r.commands, append([]string{name}, args...))
	return nil
}

func (r *stubRunner) RunWithOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	r.commands = append(r.commands, append([]string{name}, args...))
	return nil, nil
}

func (r *stubRunner) RunWithStdin(ctx context.Context, stdin []byte, name string, args ...string) error {
	r.commands = append(r.commands, append([]string{name}, args...))
	return nil
}

type admittedCommand struct {
	argv  []string
	class pressure.WorkClass
}

type mountedFreezeRunner struct {
	executed []admittedCommand
}

func (r *mountedFreezeRunner) Run(ctx context.Context, name string, args ...string) error {
	class := pressure.WorkClassFromContext(ctx, pressure.WorkNetworkProbe)
	r.executed = append(r.executed, admittedCommand{
		argv:  append([]string{name}, args...),
		class: class,
	})
	return nil
}

func (r *mountedFreezeRunner) RunWithOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	return nil, r.Run(ctx, name, args...)
}

func (r *mountedFreezeRunner) RunWithStdin(ctx context.Context, _ []byte, name string, args ...string) error {
	return r.Run(ctx, name, args...)
}

type thawRecorder struct {
	mu              sync.Mutex
	openCount       int
	freezeCount     int
	thawCount       int
	closeCount      int
	freezeErr       error
	thawErr         error
	freezeSucceeded chan struct{}
	releaseFreeze   <-chan struct{}
	thawStarted     chan struct{}
	releaseThaw     <-chan struct{}
}

func (r *thawRecorder) coordinator() *controlPlaneThawCoordinator {
	return newControlPlaneThawCoordinator(r.ops())
}

func (r *thawRecorder) persistentCoordinator(intentPath string) *controlPlaneThawCoordinator {
	return newPersistentControlPlaneThawCoordinator(r.ops(), intentPath)
}

func (r *thawRecorder) ops() controlPlaneThawOps {
	return controlPlaneThawOps{
		open: func(string) (int, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.openCount++
			return 42, nil
		},
		freeze: func(int) error {
			r.mu.Lock()
			r.freezeCount++
			freezeSucceeded := r.freezeSucceeded
			releaseFreeze := r.releaseFreeze
			freezeErr := r.freezeErr
			r.mu.Unlock()
			if freezeSucceeded != nil {
				close(freezeSucceeded)
			}
			if releaseFreeze != nil {
				<-releaseFreeze
			}
			return freezeErr
		},
		thaw: func(int) error {
			r.mu.Lock()
			r.thawCount++
			thawStarted := r.thawStarted
			releaseThaw := r.releaseThaw
			thawErr := r.thawErr
			r.mu.Unlock()
			if thawStarted != nil {
				close(thawStarted)
			}
			if releaseThaw != nil {
				<-releaseThaw
			}
			return thawErr
		},
		close: func(int) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.closeCount++
			return nil
		},
	}
}

func (r *thawRecorder) counts() (open, freeze, thaw, close int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.openCount, r.freezeCount, r.thawCount, r.closeCount
}

func assertThawCounts(t *testing.T, recorder *thawRecorder, wantOpen, wantFreeze, wantThaw, wantClose int) {
	t.Helper()
	open, freeze, thaw, closeCount := recorder.counts()
	if open != wantOpen || freeze != wantFreeze || thaw != wantThaw || closeCount != wantClose {
		t.Fatalf("freeze/thaw lifecycle open/freeze/thaw/close=%d/%d/%d/%d, want %d/%d/%d/%d",
			open, freeze, thaw, closeCount, wantOpen, wantFreeze, wantThaw, wantClose)
	}
}

func setupTestCoreRoot(t *testing.T) string {
	t.Helper()
	coreRoot := t.TempDir()
	paths.SetCoreRootForTest(t, coreRoot)

	dirs := []string{
		"crypto",
		"volumes/control-plane",
		"mounts/control-plane",
		"recovery",
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(coreRoot, d), 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	// Write a dummy LUKS loop file (simulates the encrypted control plane).
	if err := os.WriteFile(
		filepath.Join(coreRoot, "control-plane.luks"),
		[]byte("LUKS-test-data-placeholder"), 0o600,
	); err != nil {
		t.Fatal(err)
	}

	// Write essential crypto files.
	if err := os.WriteFile(
		filepath.Join(coreRoot, "crypto", "keyset.json"),
		[]byte(`{"sdek":"test","salt":"test","nonce":"test","kdf":{}}`), 0o600,
	); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(
		filepath.Join(coreRoot, "volumes", "control-plane", "piccolo.volume.json"),
		[]byte(`{"passphrase":"test"}`), 0o600,
	); err != nil {
		t.Fatal(err)
	}

	return coreRoot
}

func newTestPublisher(t *testing.T) (*Publisher, string) {
	t.Helper()
	coreRoot := setupTestCoreRoot(t)

	bus := events.NewBus()
	t.Cleanup(func() { bus.Close() })

	pub := NewPublisher(bus, &stubRunner{})
	pub.nodeID = "test-node-1234"
	pub.devFallback = true
	pub.mu.Lock()
	pub.active = true
	pub.mu.Unlock()

	return pub, coreRoot
}

func TestPublisher_PublishNow(t *testing.T) {
	pub, coreRoot := newTestPublisher(t)

	manifest, err := pub.PublishNow(context.Background())
	if err != nil {
		t.Fatalf("PublishNow: %v", err)
	}

	if manifest.Version != manifestVersion {
		t.Errorf("version = %d, want %d", manifest.Version, manifestVersion)
	}
	if manifest.Gen == "" {
		t.Error("generation ID is empty")
	}
	if manifest.SHA256 == "" {
		t.Error("SHA256 is empty")
	}
	if manifest.SizeBytes <= 0 {
		t.Error("size_bytes <= 0")
	}
	if manifest.SourceNodeID != "test-node-1234" {
		t.Errorf("node_id = %q, want %q", manifest.SourceNodeID, "test-node-1234")
	}

	// Verify archive exists.
	if _, err := os.Stat(filepath.Join(coreRoot, "recovery", "current.enc")); err != nil {
		t.Fatalf("archive not found: %v", err)
	}

	// Verify manifest file.
	data, err := os.ReadFile(filepath.Join(coreRoot, "recovery", "current.json"))
	if err != nil {
		t.Fatalf("manifest not found: %v", err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	if m.Gen != manifest.Gen {
		t.Errorf("manifest gen mismatch: file=%q, returned=%q", m.Gen, manifest.Gen)
	}
}

func TestPublisher_GenerationMonotonic(t *testing.T) {
	pub := &Publisher{}

	gen1 := pub.nextGeneration()
	gen2 := pub.nextGeneration()
	gen3 := pub.nextGeneration()

	if gen2 <= gen1 {
		t.Errorf("gen2 (%s) <= gen1 (%s)", gen2, gen1)
	}
	if gen3 <= gen2 {
		t.Errorf("gen3 (%s) <= gen2 (%s)", gen3, gen2)
	}
}

func TestPublisher_GenerationClockSkew(t *testing.T) {
	pub := &Publisher{}

	pub.mu.Lock()
	pub.lastGen = "29991231T235959Z-000010"
	pub.counter = 10
	pub.mu.Unlock()

	gen := pub.nextGeneration()
	if gen <= "29991231T235959Z-000010" {
		t.Errorf("gen should be > lastGen, got %s", gen)
	}
	if !strings.HasPrefix(gen, "29991231T235959Z-") {
		t.Errorf("expected future timestamp prefix, got %s", gen)
	}
}

func TestPublisher_HistoryPruning(t *testing.T) {
	dir := t.TempDir()

	for i := 0; i < historyLimit+3; i++ {
		name := fmt.Sprintf("20260101T%06dZ-000001", i)
		os.WriteFile(filepath.Join(dir, name+".enc"), []byte("archive"), 0o600)
		os.WriteFile(filepath.Join(dir, name+".json"), []byte("{}"), 0o600)
	}

	pub := &Publisher{}
	pub.pruneHistory(dir)

	entries, _ := os.ReadDir(dir)
	encCount := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".enc") {
			encCount++
		}
	}
	if encCount != historyLimit {
		t.Errorf("expected %d archives after pruning, got %d", historyLimit, encCount)
	}
}

func TestPublisher_DirtyLatchClears(t *testing.T) {
	pub, _ := newTestPublisher(t)

	pub.mu.Lock()
	pub.dirty = true
	pub.mu.Unlock()

	if err := pub.doPublish(context.Background()); err != nil {
		t.Fatalf("doPublish: %v", err)
	}

	pub.mu.Lock()
	if pub.dirty {
		t.Error("dirty latch not cleared after successful publish")
	}
	pub.mu.Unlock()
}

func TestCreateLoopSnapshotOrdinarySuccessThawsExactlyOnce(t *testing.T) {
	dir := t.TempDir()
	loopFile := filepath.Join(dir, "control-plane.luks")
	snapshotFile := filepath.Join(dir, "snapshot.luks")
	mountDir := filepath.Join(dir, "mount")
	contents := []byte("encrypted-control-plane")
	if err := os.WriteFile(loopFile, contents, 0o600); err != nil {
		t.Fatal(err)
	}

	run := &mountedFreezeRunner{}
	recorder := &thawRecorder{}
	pub := NewPublisher(nil, run)
	pub.thaw = recorder.coordinator()
	if err := pub.createLoopSnapshot(context.Background(), loopFile, snapshotFile, mountDir); err != nil {
		t.Fatalf("createLoopSnapshot: %v", err)
	}

	if len(run.executed) != 1 {
		t.Fatalf("snapshot child command count=%d, want only mountpoint", len(run.executed))
	}
	for _, call := range run.executed {
		if call.class != pressure.WorkStorage {
			t.Fatalf("command %v used class %q; want storage", call.argv, call.class)
		}
		if call.argv[0] != "mountpoint" {
			t.Fatalf("snapshot spawned unexpected child command: %v", call.argv)
		}
	}
	assertThawCounts(t, recorder, 1, 1, 1, 1)
	got, err := os.ReadFile(snapshotFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(contents) {
		t.Fatalf("snapshot contents = %q; want %q", got, contents)
	}
}

func TestCreateLoopSnapshotOrdinaryCopyErrorStillThaws(t *testing.T) {
	copyErr := errors.New("copy failed")
	recorder := &thawRecorder{}
	pub := NewPublisher(nil, &mountedFreezeRunner{})
	pub.thaw = recorder.coordinator()
	pub.copySnapshot = func(string, string) error { return copyErr }

	err := pub.createLoopSnapshot(context.Background(), "loop", "snapshot", "mount")
	if !errors.Is(err, copyErr) {
		t.Fatalf("createLoopSnapshot error=%v, want %v", err, copyErr)
	}
	assertThawCounts(t, recorder, 1, 1, 1, 1)
}

func TestCreateLoopSnapshotCriticalDuringCopyRacesDeferredCleanupExactOnce(t *testing.T) {
	thawStarted := make(chan struct{})
	releaseThaw := make(chan struct{})
	recorder := &thawRecorder{thawStarted: thawStarted, releaseThaw: releaseThaw}
	coordinator := recorder.coordinator()
	copyStarted := make(chan struct{})
	releaseCopy := make(chan struct{})
	pub := NewPublisher(nil, &mountedFreezeRunner{})
	pub.thaw = coordinator
	pub.copySnapshot = func(string, string) error {
		close(copyStarted)
		<-releaseCopy
		return nil
	}

	done := make(chan error, 1)
	go func() {
		done <- pub.createLoopSnapshot(context.Background(), "loop", "snapshot", "mount")
	}()
	<-copyStarted
	emergencyDone := make(chan error, 1)
	go func() { emergencyDone <- coordinator.thawActive() }()
	<-thawStarted
	close(releaseCopy)
	select {
	case err := <-done:
		t.Fatalf("ordinary cleanup returned before in-process emergency thaw completed: %v", err)
	default:
	}
	close(releaseThaw)
	if err := <-emergencyDone; err != nil {
		t.Fatalf("emergency thaw: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("createLoopSnapshot: %v", err)
	}
	assertThawCounts(t, recorder, 1, 1, 1, 1)
}

func TestCreateLoopSnapshotCriticalImmediatelyBeforeDeferredCleanupIsExactOnce(t *testing.T) {
	recorder := &thawRecorder{}
	coordinator := recorder.coordinator()
	pub := NewPublisher(nil, &mountedFreezeRunner{})
	pub.thaw = coordinator
	pub.copySnapshot = func(string, string) error {
		return coordinator.thawActive()
	}

	if err := pub.createLoopSnapshot(context.Background(), "loop", "snapshot", "mount"); err != nil {
		t.Fatalf("createLoopSnapshot: %v", err)
	}
	assertThawCounts(t, recorder, 1, 1, 1, 1)
}

func TestControlPlaneFreezeIntentClosesSuccessToRegistrationGap(t *testing.T) {
	intentPath := filepath.Join(t.TempDir(), "control-plane-freeze.intent")
	freezeSucceeded := make(chan struct{})
	releaseFreeze := make(chan struct{})
	recorder := &thawRecorder{freezeSucceeded: freezeSucceeded, releaseFreeze: releaseFreeze}
	coordinator := recorder.persistentCoordinator(intentPath)
	type freezeResult struct {
		obligation *controlPlaneThawObligation
		err        error
	}
	freezeDone := make(chan freezeResult, 1)
	go func() {
		obligation, err := coordinator.freeze("mount")
		freezeDone <- freezeResult{obligation: obligation, err: err}
	}()

	// The fake reports simulated kernel success but deliberately prevents the
	// coordinator from publishing the in-memory obligation and returning yet.
	<-freezeSucceeded
	if pending, err := controlPlaneFreezeIntentPending(intentPath); err != nil || !pending {
		t.Fatalf("freeze intent before FIFREEZE completion: pending=%v err=%v", pending, err)
	}
	fenceDone := make(chan struct{})
	go func() {
		coordinator.fenceFatal()
		close(fenceDone)
	}()
	select {
	case <-fenceDone:
	case <-time.After(time.Second):
		t.Fatal("fatal freeze fence waited for the in-flight FIFREEZE ioctl")
	}

	close(releaseFreeze)
	frozen := <-freezeDone
	if frozen.err != nil || frozen.obligation == nil {
		t.Fatalf("freeze result obligation=%v err=%v", frozen.obligation, frozen.err)
	}
	if err := frozen.obligation.thaw(); err != nil {
		t.Fatalf("ordinary exact-once cleanup: %v", err)
	}
	if pending, err := controlPlaneFreezeIntentPending(intentPath); err != nil || pending {
		t.Fatalf("freeze intent after ordinary thaw: pending=%v err=%v", pending, err)
	}
	assertThawCounts(t, recorder, 1, 1, 1, 1)
}

func TestControlPlaneFatalWinsBeforeFreeze(t *testing.T) {
	recorder := &thawRecorder{}
	coordinator := recorder.coordinator()
	coordinator.fenceFatal()
	if obligation, err := coordinator.freeze("mount"); obligation != nil || !errors.Is(err, errControlPlaneFreezeFatalFenced) {
		t.Fatalf("post-fatal freeze obligation=%v err=%v", obligation, err)
	}
	assertThawCounts(t, recorder, 0, 0, 0, 0)
}

func TestRecoverPendingControlPlaneThawClearsIntentAfterSuccessOrAlreadyThawed(t *testing.T) {
	for _, thawErr := range []error{nil, unix.EINVAL} {
		t.Run(fmt.Sprint(thawErr), func(t *testing.T) {
			intentPath := filepath.Join(t.TempDir(), "control-plane-freeze.intent")
			if err := writeControlPlaneFreezeIntent(intentPath); err != nil {
				t.Fatal(err)
			}
			recorder := &thawRecorder{thawErr: thawErr}
			if err := recoverPendingControlPlaneThaw(intentPath, "mount", recorder.ops()); err != nil {
				t.Fatalf("recover pending thaw: %v", err)
			}
			if pending, err := controlPlaneFreezeIntentPending(intentPath); err != nil || pending {
				t.Fatalf("recovered intent: pending=%v err=%v", pending, err)
			}
			assertThawCounts(t, recorder, 1, 0, 1, 1)
		})
	}
}

func TestRecoverPendingControlPlaneThawRetainsIntentOnError(t *testing.T) {
	intentPath := filepath.Join(t.TempDir(), "control-plane-freeze.intent")
	if err := writeControlPlaneFreezeIntent(intentPath); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("thaw blocked")
	recorder := &thawRecorder{thawErr: wantErr}
	err := recoverPendingControlPlaneThaw(intentPath, "mount", recorder.ops())
	if !errors.Is(err, wantErr) {
		t.Fatalf("recover pending thaw error=%v, want %v", err, wantErr)
	}
	if pending, statErr := controlPlaneFreezeIntentPending(intentPath); statErr != nil || !pending {
		t.Fatalf("failed recovery intent: pending=%v err=%v", pending, statErr)
	}
	assertThawCounts(t, recorder, 1, 0, 1, 1)
}

func TestOrdinaryControlPlaneThawRetainsIntentOnError(t *testing.T) {
	intentPath := filepath.Join(t.TempDir(), "control-plane-freeze.intent")
	wantErr := errors.New("ordinary thaw failed")
	recorder := &thawRecorder{thawErr: wantErr}
	coordinator := recorder.persistentCoordinator(intentPath)
	obligation, err := coordinator.freeze("mount")
	if err != nil {
		t.Fatalf("freeze: %v", err)
	}
	if err := obligation.thaw(); !errors.Is(err, wantErr) {
		t.Fatalf("ordinary thaw error=%v, want %v", err, wantErr)
	}
	if pending, statErr := controlPlaneFreezeIntentPending(intentPath); statErr != nil || !pending {
		t.Fatalf("failed ordinary thaw intent: pending=%v err=%v", pending, statErr)
	}
	assertThawCounts(t, recorder, 1, 1, 1, 1)
}

func TestControlPlaneFreezeFailureClearsIntent(t *testing.T) {
	intentPath := filepath.Join(t.TempDir(), "control-plane-freeze.intent")
	wantErr := errors.New("freeze failed")
	recorder := &thawRecorder{freezeErr: wantErr}
	coordinator := recorder.persistentCoordinator(intentPath)
	if obligation, err := coordinator.freeze("mount"); obligation != nil || !errors.Is(err, wantErr) {
		t.Fatalf("freeze obligation=%v err=%v, want %v", obligation, err, wantErr)
	}
	if pending, statErr := controlPlaneFreezeIntentPending(intentPath); statErr != nil || pending {
		t.Fatalf("failed freeze intent: pending=%v err=%v", pending, statErr)
	}
	assertThawCounts(t, recorder, 1, 1, 0, 1)
}

func TestControlPlaneFreezeFailureClosesWithoutThaw(t *testing.T) {
	freezeErr := errors.New("freeze failed")
	recorder := &thawRecorder{freezeErr: freezeErr}
	coordinator := recorder.coordinator()
	if obligation, err := coordinator.freeze("mount"); obligation != nil || !errors.Is(err, freezeErr) {
		t.Fatalf("freeze obligation=%v err=%v, want %v", obligation, err, freezeErr)
	}
	if err := coordinator.thawActive(); err != nil {
		t.Fatalf("fatal cleanup after failed freeze: %v", err)
	}
	assertThawCounts(t, recorder, 1, 1, 0, 1)
}

func TestPublisher_StopSkipsFlush(t *testing.T) {
	pub, coreRoot := newTestPublisher(t)

	pub.mu.Lock()
	pub.dirty = true
	pub.mu.Unlock()

	if err := pub.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if _, err := os.Stat(filepath.Join(coreRoot, "recovery", "current.enc")); err == nil {
		t.Fatal("Stop should not produce archive — flush-on-shutdown was removed")
	}
}

func TestPublisher_SizeGuard(t *testing.T) {
	pub, coreRoot := newTestPublisher(t)

	// Write a large random (incompressible) loop file to trigger size guard.
	largeData := make([]byte, maxPCVSize+1)
	rand.Read(largeData)
	if err := os.WriteFile(
		filepath.Join(coreRoot, "control-plane.luks"),
		largeData, 0o600,
	); err != nil {
		t.Fatal(err)
	}

	failCh := pub.bus.Subscribe(events.TopicPCVExportFailed, 4)

	err := pub.doPublish(context.Background())
	if err == nil {
		t.Fatal("expected error for oversized archive")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("unexpected error: %v", err)
	}

	select {
	case <-failCh:
		// ok
	case <-time.After(time.Second):
		t.Error("expected PCVExportFailed event")
	}
}

func TestPublisher_NotActiveReturnsError(t *testing.T) {
	bus := events.NewBus()
	defer bus.Close()

	pub := NewPublisher(bus, &stubRunner{})
	_, err := pub.PublishNow(context.Background())
	if err == nil {
		t.Fatal("expected error when not active")
	}
}

func TestPublisher_HistoryArchivesCreated(t *testing.T) {
	pub, coreRoot := newTestPublisher(t)

	// Publish twice.
	if _, err := pub.PublishNow(context.Background()); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	if _, err := pub.PublishNow(context.Background()); err != nil {
		t.Fatalf("second publish: %v", err)
	}

	historyDir := filepath.Join(coreRoot, "recovery", "history")
	entries, err := os.ReadDir(historyDir)
	if err != nil {
		t.Fatalf("read history dir: %v", err)
	}

	encCount := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".enc") {
			encCount++
		}
	}
	if encCount < 2 {
		t.Errorf("expected at least 2 history archives, got %d", encCount)
	}
}
