package autounlock

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"piccolod/internal/state/paths"
)

type taskPressureIntentFixture struct {
	mu     sync.Mutex
	latest TaskPressureIntent
}

func (f *taskPressureIntentFixture) set(intent TaskPressureIntent) {
	f.mu.Lock()
	f.latest = intent
	f.mu.Unlock()
}

func (f *taskPressureIntentFixture) view() TaskPressureIntent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.latest
}

func newTaskPressureOrchestrator(
	t *testing.T,
	manager ManagerOps,
	provider RecoveryFactorProvider,
	now time.Time,
) *Orchestrator {
	t.Helper()
	paths.SetCoreRootForTest(t, t.TempDir())
	o, err := New(Deps{
		Manager:                 manager,
		RecoveryProvider:        func() RecoveryFactorProvider { return provider },
		RecoveryProviderID:      "pressure-test-provider",
		GetDeviceID:             func() string { return "pressure-test-device" },
		IsRecoveryProviderReady: func() bool { return true },
		IsIdentityReady:         func() bool { return true },
		Now:                     func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestTaskPressureWarningToNormalDuringDepositClearsLocalHandoff(t *testing.T) {
	now := time.Date(2026, 7, 19, 11, 42, 0, 0, time.UTC)
	manager := &opaqueRecordingManager{loaded: true, blob: []byte("opaque-warning-normal-blob")}
	provider := &observingProvider{
		expiry:  now.Add(taskWarningHandoffTTL),
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	o := newTaskPressureOrchestrator(t, manager, provider, now)
	view := &taskPressureIntentFixture{latest: TaskPressureIntent{State: TaskPressureIntentWarning, Generation: 1}}

	done := make(chan error, 1)
	go func() {
		done <- o.ReconcileTaskPressureIntent(context.Background(), view.view(), view.view)
	}()
	select {
	case <-provider.started:
	case <-time.After(time.Second):
		t.Fatal("Warning reconciliation did not reach provider")
	}

	view.set(TaskPressureIntent{State: TaskPressureIntentNormal, Generation: 2})
	close(provider.release)
	if err := <-done; err != nil {
		t.Fatalf("ReconcileTaskPressureIntent: %v", err)
	}
	if BlobExists() {
		t.Fatal("stale Warning completion retained raw blob after Normal")
	}
	state, err := LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Handoff) != 0 {
		t.Fatalf("stale Warning completion retained metadata after Normal: %s", state.Handoff)
	}
	if provider.depositCount != 1 {
		t.Fatalf("deposit count = %d; want 1", provider.depositCount)
	}
	if o.taskWarningHandoffClaimDigest != "" {
		t.Fatalf("Normal retained Warning ownership %q", o.taskWarningHandoffClaimDigest)
	}
}

func TestTaskPressureWarningToNormalPreservesUnownedExistingHandoff(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata func(t *testing.T, o *Orchestrator, blob []byte) []byte
	}{
		{name: "legacy expiry unknown"},
		{
			name: "recognized lifetime below minimum",
			metadata: func(t *testing.T, o *Orchestrator, blob []byte) []byte {
				t.Helper()
				state := State{}
				if err := setHandoffMetadata(&state, "pressure-test-provider", o.deps.Now().Add(minimumEffectiveLifetime-time.Second), blob); err != nil {
					t.Fatal(err)
				}
				return state.Handoff
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 7, 19, 11, 42, 30, 0, time.UTC)
			manager := &opaqueRecordingManager{loaded: true, blob: []byte("unused-replacement")}
			provider := &observingProvider{expiry: now.Add(taskWarningHandoffTTL)}
			o := newTaskPressureOrchestrator(t, manager, provider, now)
			blob := []byte("pre-existing-unowned-handoff")
			if err := WriteBlob(blob); err != nil {
				t.Fatal(err)
			}
			state := State{Enabled: true}
			if tc.metadata != nil {
				state.Handoff = tc.metadata(t, o, blob)
			}
			if err := SaveState(state); err != nil {
				t.Fatal(err)
			}
			beforeState, err := os.ReadFile(statePath())
			if err != nil {
				t.Fatal(err)
			}
			view := &taskPressureIntentFixture{latest: TaskPressureIntent{State: TaskPressureIntentWarning, Generation: 1}}

			if err := o.ReconcileTaskPressureIntent(context.Background(), view.view(), view.view); err != nil {
				t.Fatalf("Warning reconciliation: %v", err)
			}
			if o.taskWarningHandoffClaimDigest != "" || provider.depositCount != 0 || manager.factor != nil {
				t.Fatalf("Warning claimed or replaced pre-existing handoff: claim=%q deposits=%d wraps=%t", o.taskWarningHandoffClaimDigest, provider.depositCount, manager.factor != nil)
			}

			view.set(TaskPressureIntent{State: TaskPressureIntentNormal, Generation: 2})
			if err := o.ReconcileTaskPressureIntent(context.Background(), view.view(), view.view); err != nil {
				t.Fatalf("Normal reconciliation: %v", err)
			}
			gotBlob, err := ReadBlob()
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(gotBlob, blob) {
				t.Fatalf("Normal changed pre-existing blob: %q", gotBlob)
			}
			afterState, err := os.ReadFile(statePath())
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(afterState, beforeState) {
				t.Fatalf("Normal changed pre-existing metadata: got %s, want %s", afterState, beforeState)
			}
		})
	}
}

func TestTaskPressureWarningReuseRetainsExactOwnershipUntilNormal(t *testing.T) {
	now := time.Date(2026, 7, 19, 11, 42, 45, 0, time.UTC)
	manager := &opaqueRecordingManager{loaded: true, blob: []byte("warning-reuse-handoff")}
	provider := &observingProvider{expiry: now.Add(taskWarningHandoffTTL)}
	o := newTaskPressureOrchestrator(t, manager, provider, now)
	view := &taskPressureIntentFixture{latest: TaskPressureIntent{State: TaskPressureIntentWarning, Generation: 1}}

	if err := o.ReconcileTaskPressureIntent(context.Background(), view.view(), view.view); err != nil {
		t.Fatalf("first Warning reconciliation: %v", err)
	}
	warningDigest := o.taskWarningHandoffClaimDigest
	if warningDigest == "" {
		t.Fatal("fresh Warning handoff was not owned")
	}
	view.set(TaskPressureIntent{State: TaskPressureIntentWarning, Generation: 2})
	if err := o.ReconcileTaskPressureIntent(context.Background(), view.view(), view.view); err != nil {
		t.Fatalf("second Warning reconciliation: %v", err)
	}
	if provider.depositCount != 1 || o.taskWarningHandoffClaimDigest != warningDigest {
		t.Fatalf("Warning reuse changed ownership: deposits=%d claim=%q want=%q", provider.depositCount, o.taskWarningHandoffClaimDigest, warningDigest)
	}

	view.set(TaskPressureIntent{State: TaskPressureIntentNormal, Generation: 3})
	if err := o.ReconcileTaskPressureIntent(context.Background(), view.view(), view.view); err != nil {
		t.Fatalf("Normal reconciliation: %v", err)
	}
	if BlobExists() || o.taskWarningHandoffClaimDigest != "" {
		t.Fatalf("Normal retained Warning authority: blob=%t claim=%q", BlobExists(), o.taskWarningHandoffClaimDigest)
	}
}

func TestTaskPressureNormalDoesNotTransferWarningOwnershipToReplacement(t *testing.T) {
	now := time.Date(2026, 7, 19, 11, 42, 50, 0, time.UTC)
	manager := &opaqueRecordingManager{loaded: true, blob: []byte("warning-owned-handoff")}
	provider := &observingProvider{expiry: now.Add(taskWarningHandoffTTL)}
	o := newTaskPressureOrchestrator(t, manager, provider, now)
	view := &taskPressureIntentFixture{latest: TaskPressureIntent{State: TaskPressureIntentWarning, Generation: 1}}
	if err := o.ReconcileTaskPressureIntent(context.Background(), view.view(), view.view); err != nil {
		t.Fatalf("Warning reconciliation: %v", err)
	}

	replacement := []byte("later-unrelated-replacement")
	if err := WriteBlob(replacement); err != nil {
		t.Fatal(err)
	}
	state := State{Enabled: true}
	if err := setHandoffMetadata(&state, "pressure-test-provider", now.Add(taskWarningHandoffTTL), replacement); err != nil {
		t.Fatal(err)
	}
	if err := SaveState(state); err != nil {
		t.Fatal(err)
	}
	view.set(TaskPressureIntent{State: TaskPressureIntentNormal, Generation: 2})
	if err := o.ReconcileTaskPressureIntent(context.Background(), view.view(), view.view); err != nil {
		t.Fatalf("Normal reconciliation: %v", err)
	}
	got, err := ReadBlob()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, replacement) || o.taskWarningHandoffClaimDigest != "" {
		t.Fatalf("Normal consumed or claimed replacement: blob=%q claim=%q", got, o.taskWarningHandoffClaimDigest)
	}
}

func TestTaskPressureNormalCleansWarningBlobAfterNamekMetadataSaveFailure(t *testing.T) {
	o, _, namek, _ := newOrchestrator(t)
	var restore func() error
	var hookErr error
	namek.onDeposit = func() {
		restore, hookErr = forceNextStateSaveFailure()
	}
	result, err := o.Prepare(context.Background(), PrepareTriggerTaskWarning, taskWarningHandoffTTL)
	if err == nil || result.Disposition != PrepareDispositionUnavailable {
		t.Fatalf("Warning Prepare = %+v, %v; want unavailable metadata-save failure", result, err)
	}
	if hookErr != nil {
		t.Fatalf("install metadata-save failure: %v", hookErr)
	}
	if restore == nil {
		t.Fatal("Namek deposit did not install metadata-save failure")
	}
	if err := restore(); err != nil {
		t.Fatalf("restore preparing metadata: %v", err)
	}
	blob, readErr := ReadBlob()
	if readErr != nil {
		t.Fatalf("Namek fallback blob: %v", readErr)
	}
	if o.taskWarningHandoffClaimDigest != rawBlobDigest(blob) || o.restartHandoffClaimDigest != "" {
		t.Fatalf("failed Warning ownership = warning %q restart %q", o.taskWarningHandoffClaimDigest, o.restartHandoffClaimDigest)
	}

	view := &taskPressureIntentFixture{latest: TaskPressureIntent{State: TaskPressureIntentNormal, Generation: 1}}
	if err := o.ReconcileTaskPressureIntent(context.Background(), view.view(), view.view); err != nil {
		t.Fatalf("Normal reconciliation: %v", err)
	}
	if BlobExists() || o.taskWarningHandoffClaimDigest != "" {
		t.Fatalf("Normal retained failed Warning authority: blob=%t claim=%q", BlobExists(), o.taskWarningHandoffClaimDigest)
	}
}

func TestTaskPressureQueuedNormalDoesNotCancelCriticalHandoff(t *testing.T) {
	now := time.Date(2026, 7, 19, 11, 43, 0, 0, time.UTC)
	manager := &opaqueRecordingManager{loaded: true, blob: []byte("opaque-critical-handoff")}
	provider := &observingProvider{
		expiry:  now.Add(taskWarningHandoffTTL),
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	o := newTaskPressureOrchestrator(t, manager, provider, now)

	criticalPrepareDone := make(chan error, 1)
	go func() {
		_, err := o.Prepare(context.Background(), PrepareTriggerTaskCritical, taskWarningHandoffTTL)
		criticalPrepareDone <- err
	}()
	select {
	case <-provider.started:
	case <-time.After(time.Second):
		t.Fatal("Critical-like Prepare did not acquire operation gate")
	}

	view := &taskPressureIntentFixture{latest: TaskPressureIntent{State: TaskPressureIntentNormal, Generation: 1}}
	normalStarted := make(chan struct{})
	normalDone := make(chan error, 1)
	go func() {
		close(normalStarted)
		normalDone <- o.ReconcileTaskPressureIntent(
			context.Background(),
			TaskPressureIntent{State: TaskPressureIntentNormal, Generation: 1},
			view.view,
		)
	}()
	<-normalStarted
	view.set(TaskPressureIntent{State: TaskPressureIntentCritical, Generation: 2})
	close(provider.release)

	if err := <-criticalPrepareDone; err != nil {
		t.Fatalf("Critical-like Prepare: %v", err)
	}
	if err := <-normalDone; err != nil {
		t.Fatalf("queued Normal reconciliation: %v", err)
	}
	if !BlobExists() {
		t.Fatal("stale queued Normal canceled the Critical handoff")
	}
	state, err := LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Handoff) == 0 {
		t.Fatal("stale queued Normal canceled Critical handoff metadata")
	}
	if provider.depositCount != 1 || manager.factor == nil {
		t.Fatalf("Critical intent performed unexpected provider work: deposits=%d wraps=%t", provider.depositCount, manager.factor != nil)
	}
}

func TestTaskPressureWarningToCriticalRetainsPreparedHandoff(t *testing.T) {
	now := time.Date(2026, 7, 19, 11, 44, 0, 0, time.UTC)
	manager := &opaqueRecordingManager{loaded: true, blob: []byte("opaque-warning-critical-blob")}
	provider := &observingProvider{
		expiry:  now.Add(taskWarningHandoffTTL),
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	o := newTaskPressureOrchestrator(t, manager, provider, now)
	view := &taskPressureIntentFixture{latest: TaskPressureIntent{State: TaskPressureIntentWarning, Generation: 1}}

	done := make(chan error, 1)
	go func() {
		done <- o.ReconcileTaskPressureIntent(context.Background(), view.view(), view.view)
	}()
	select {
	case <-provider.started:
	case <-time.After(time.Second):
		t.Fatal("Warning reconciliation did not reach provider")
	}

	view.set(TaskPressureIntent{State: TaskPressureIntentCritical, Generation: 2})
	close(provider.release)
	if err := <-done; err != nil {
		t.Fatalf("ReconcileTaskPressureIntent: %v", err)
	}
	if !BlobExists() {
		t.Fatal("Warning completion discarded handoff after Critical")
	}
	state, err := LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Handoff) == 0 {
		t.Fatal("Warning completion discarded metadata after Critical")
	}
	if provider.depositCount != 1 {
		t.Fatalf("Critical caused a background provider call; deposits=%d", provider.depositCount)
	}
}

func TestTaskPressureQueuedNormalCannotCancelGracefulRestartHandoff(t *testing.T) {
	now := time.Date(2026, 7, 19, 11, 44, 30, 0, time.UTC)
	manager := &opaqueRecordingManager{loaded: true, blob: []byte("opaque-graceful-claim")}
	provider := &observingProvider{expiry: now.Add(taskWarningHandoffTTL)}
	o := newTaskPressureOrchestrator(t, manager, provider, now)
	view := &taskPressureIntentFixture{latest: TaskPressureIntent{State: TaskPressureIntentWarning, Generation: 1}}

	if err := o.ReconcileTaskPressureIntent(context.Background(), view.view(), view.view); err != nil {
		t.Fatalf("Warning reconciliation: %v", err)
	}
	if provider.depositCount != 1 || manager.factor == nil {
		t.Fatalf("Warning did not prepare handoff: deposits=%d wraps=%t", provider.depositCount, manager.factor != nil)
	}

	// Model a Normal callback already queued by the relay, but not admitted to
	// continuity until after graceful shutdown has reused and claimed the
	// Warning handoff.
	view.set(TaskPressureIntent{State: TaskPressureIntentNormal, Generation: 2})
	normalQueued := make(chan struct{})
	allowNormal := make(chan struct{})
	normalDone := make(chan error, 1)
	go func() {
		close(normalQueued)
		<-allowNormal
		normalDone <- o.ReconcileTaskPressureIntent(
			context.Background(),
			TaskPressureIntent{State: TaskPressureIntentNormal, Generation: 2},
			view.view,
		)
	}()
	<-normalQueued

	if err := o.RunCeremony(context.Background()); err != nil {
		t.Fatalf("RunCeremony: %v", err)
	}
	if provider.depositCount != 1 {
		t.Fatalf("graceful restart replaced reusable Warning handoff; deposits=%d", provider.depositCount)
	}
	if o.restartHandoffClaimDigest == "" || o.restartHandoffClaimDigest != o.taskWarningHandoffClaimDigest {
		t.Fatalf("graceful claim did not take precedence for the Warning blob: restart=%q warning=%q", o.restartHandoffClaimDigest, o.taskWarningHandoffClaimDigest)
	}
	close(allowNormal)
	if err := <-normalDone; err != nil {
		t.Fatalf("queued Normal reconciliation: %v", err)
	}

	if !BlobExists() {
		t.Fatal("queued Normal canceled graceful restart raw handoff")
	}
	state, err := LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Handoff) == 0 {
		t.Fatal("queued Normal canceled graceful restart handoff metadata")
	}
}

func TestTaskPressureNormalCannotCancelFatalRestartHandoff(t *testing.T) {
	for _, trigger := range []PrepareTrigger{PrepareTriggerTaskCritical, PrepareTriggerRecoveryFatal} {
		t.Run(string(trigger), func(t *testing.T) {
			now := time.Date(2026, 7, 19, 11, 44, 45, 0, time.UTC)
			manager := &opaqueRecordingManager{loaded: true, blob: []byte("opaque-fatal-claim")}
			provider := &observingProvider{expiry: now.Add(taskWarningHandoffTTL)}
			o := newTaskPressureOrchestrator(t, manager, provider, now)
			view := &taskPressureIntentFixture{latest: TaskPressureIntent{State: TaskPressureIntentWarning, Generation: 1}}

			if err := o.ReconcileTaskPressureIntent(context.Background(), view.view(), view.view); err != nil {
				t.Fatalf("Warning reconciliation: %v", err)
			}
			result, err := o.Prepare(context.Background(), trigger, taskWarningHandoffTTL)
			if err != nil || result.Disposition != PrepareDispositionReused {
				t.Fatalf("restart Prepare = %+v, %v; want reused", result, err)
			}
			if o.restartHandoffClaimDigest == "" || o.restartHandoffClaimDigest != o.taskWarningHandoffClaimDigest {
				t.Fatalf("%s claim did not take precedence for the Warning blob: restart=%q warning=%q", trigger, o.restartHandoffClaimDigest, o.taskWarningHandoffClaimDigest)
			}

			view.set(TaskPressureIntent{State: TaskPressureIntentNormal, Generation: 2})
			if err := o.ReconcileTaskPressureIntent(context.Background(), view.view(), view.view); err != nil {
				t.Fatalf("Normal reconciliation: %v", err)
			}
			if !BlobExists() {
				t.Fatalf("Normal canceled %s raw handoff", trigger)
			}
			state, err := LoadState()
			if err != nil {
				t.Fatal(err)
			}
			if len(state.Handoff) == 0 {
				t.Fatalf("Normal canceled %s handoff metadata", trigger)
			}
		})
	}
}

func TestUnavailableRestartPrepareDoesNotClaimTaskWarningHandoff(t *testing.T) {
	now := time.Date(2026, 7, 19, 11, 44, 50, 0, time.UTC)
	manager := &opaqueRecordingManager{loaded: true, blob: []byte("opaque-unavailable-claim")}
	provider := &observingProvider{expiry: now.Add(taskWarningHandoffTTL)}
	o := newTaskPressureOrchestrator(t, manager, provider, now)
	view := &taskPressureIntentFixture{latest: TaskPressureIntent{State: TaskPressureIntentWarning, Generation: 1}}

	if err := o.ReconcileTaskPressureIntent(context.Background(), view.view(), view.view); err != nil {
		t.Fatalf("Warning reconciliation: %v", err)
	}
	blob, err := ReadBlob()
	if err != nil {
		t.Fatal(err)
	}
	state, err := LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if err := setHandoffMetadata(&state, "pressure-test-provider", now.Add(minimumEffectiveLifetime-time.Second), blob); err != nil {
		t.Fatal(err)
	}
	if err := SaveState(state); err != nil {
		t.Fatal(err)
	}
	o.deps.IsRecoveryProviderReady = func() bool { return false }

	result, err := o.Prepare(context.Background(), PrepareTriggerGracefulShutdown, taskWarningHandoffTTL)
	if err != nil {
		t.Fatalf("graceful Prepare: %v", err)
	}
	if result.Disposition != PrepareDispositionUnavailable {
		t.Fatalf("graceful Prepare disposition = %q; want unavailable", result.Disposition)
	}

	view.set(TaskPressureIntent{State: TaskPressureIntentNormal, Generation: 2})
	if err := o.ReconcileTaskPressureIntent(context.Background(), view.view(), view.view); err != nil {
		t.Fatalf("Normal reconciliation: %v", err)
	}
	if BlobExists() {
		t.Fatal("unavailable restart Prepare claimed Warning raw handoff")
	}
	state, err = LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Handoff) != 0 {
		t.Fatalf("unavailable restart Prepare retained claimed metadata: %s", state.Handoff)
	}
}

type revokeObservingProvider struct {
	observingProvider
	revokeCount atomic.Int32
}

// Revoke is deliberately outside RecoveryFactorProvider. Its presence lets the
// regression test prove pressure cleanup does not discover or invoke an
// unkeyed provider extension by type assertion.
func (p *revokeObservingProvider) Revoke(context.Context) error {
	p.revokeCount.Add(1)
	return nil
}

func TestTaskPressureHandoffPersistsNoPlaintextAndNormalNeverRevokes(t *testing.T) {
	now := time.Date(2026, 7, 19, 11, 45, 0, 0, time.UTC)
	manager := &opaqueRecordingManager{
		loaded: true,
		blob:   []byte("opaque-pressure-aead-blob"),
		sdek:   []byte("pressure-plaintext-sdek-must-not-persist"),
	}
	provider := &revokeObservingProvider{observingProvider: observingProvider{
		expiry: now.Add(taskWarningHandoffTTL),
	}}
	o := newTaskPressureOrchestrator(t, manager, provider, now)
	view := &taskPressureIntentFixture{latest: TaskPressureIntent{State: TaskPressureIntentWarning, Generation: 1}}

	if err := o.ReconcileTaskPressureIntent(context.Background(), view.view(), view.view); err != nil {
		t.Fatalf("Warning reconciliation: %v", err)
	}
	rawBlob, err := ReadBlob()
	if err != nil {
		t.Fatal(err)
	}
	stateBytes, err := os.ReadFile(statePath())
	if err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	factor := append([]byte(nil), provider.factor...)
	provider.mu.Unlock()
	for name, secret := range map[string][]byte{
		"factor raw":    factor,
		"factor base64": []byte(base64.RawURLEncoding.EncodeToString(factor)),
		"factor hex":    []byte(hex.EncodeToString(factor)),
		"SDEK":          manager.sdek,
	} {
		if len(secret) != 0 && (bytes.Contains(stateBytes, secret) || bytes.Contains(rawBlob, secret)) {
			t.Fatalf("%s persisted in task-pressure handoff", name)
		}
	}

	view.set(TaskPressureIntent{State: TaskPressureIntentNormal, Generation: 2})
	if err := o.ReconcileTaskPressureIntent(
		context.Background(),
		TaskPressureIntent{State: TaskPressureIntentNormal, Generation: 2},
		view.view,
	); err != nil {
		t.Fatalf("Normal reconciliation: %v", err)
	}
	if provider.revokeCount.Load() != 0 {
		t.Fatalf("Normal cleanup called unkeyed Revoke %d times", provider.revokeCount.Load())
	}
	if BlobExists() {
		t.Fatal("Normal cleanup retained raw blob")
	}
}
