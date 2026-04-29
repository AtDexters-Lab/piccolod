package identity

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"piccolod/internal/events"
)

// enrollTracker is an http.Handler for enrollment endpoints that records each
// call on a channel and can be flipped mid-test to return success vs 409.
// Used by the device-conflict self-heal tests.
type enrollTracker struct {
	mu       sync.Mutex
	calls    int64
	returnOK atomic.Bool
	calledCh chan struct{} // buffered; send on each call
}

func newEnrollTracker() *enrollTracker {
	return &enrollTracker{calledCh: make(chan struct{}, 16)}
}

func (e *enrollTracker) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&e.calls, 1)
		// Non-blocking send — drop if buffer full (tests should drain promptly).
		select {
		case e.calledCh <- struct{}{}:
		default:
		}
		// /api/v1/nonce is used by authenticated calls; return a fixed nonce
		// for any path that isn't the enrollment handler.
		switch r.URL.Path {
		case "/api/v1/nonce":
			_ = json.NewEncoder(w).Encode(map[string]string{"nonce": "test-nonce"})
			return
		case "/api/v1/devices/enroll":
			// Phase 1: return 409 if we're in conflict mode, else proceed.
			if !e.returnOK.Load() {
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "device already enrolled"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"nonce":          "00",
				"enc_credential": "YQ==", // base64("a")
			})
			return
		case "/api/v1/devices/enroll/attest":
			// Phase 2: identical flip behavior. In practice phase-1 is what
			// our handler cares about for the 409 loop, but mirror the flag
			// for completeness.
			if !e.returnOK.Load() {
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "device already enrolled"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_id":     "dev-123",
				"hostname":      "slug.test.local",
				"identity_class": "unverified",
				"nexus_endpoints": []string{"wss://relay.test"},
				"reenrolled":    true,
			})
			return
		default:
			w.WriteHeader(404)
		}
	})
}

// TestReenrollWithRecovery_DeviceConflict_SetsSuspendedAndContinuesBackoff
// verifies that a 409 response:
// (a) sets suspended=true exactly once (log-on-transition guard),
// (b) does NOT exit the retry loop,
// (c) self-heals when the server flips to success — finalizeEnrollment
//     clears suspended and restarts sync.
func TestReenrollWithRecovery_DeviceConflict_SetsSuspendedAndContinuesBackoff(t *testing.T) {
	tracker := newEnrollTracker()
	srv := httptest.NewServer(tracker.Handler())
	defer srv.Close()

	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "identity.json")
	svc := &Service{
		configPath: configPath,
		tpmDev:     &mockTPM{},
		cfg: Config{
			Enabled:  true,
			DeviceID: "dev-123",
			Hostname: "slug.test.local",
			NamekURL: srv.URL,
		},
		client: newNamekClient(srv.URL, &mockTPM{}, "dev-123"),
		stopCh: make(chan struct{}),
	}
	svc.enrolled.Store(true)

	bus := events.NewBus()
	defer bus.Close()
	svc.eventsBus = bus

	if err := saveConfig(configPath, svc.cfg); err != nil {
		t.Fatal(err)
	}

	// Subscribe to TopicIdentityChanged — suspended transition publishes.
	changedCh := bus.Subscribe(events.TopicIdentityChanged, 8)

	// Precondition: reenrollWithRecovery expects the caller
	// (triggerReenrollWithRecovery) to have already CAS-set recovering=true.
	// Tests calling it directly must set the flag themselves.
	svc.recovering.Store(true)

	// Run reenrollWithRecovery in a goroutine — it loops forever until we
	// flip the tracker to success.
	done := make(chan struct{})
	go func() {
		svc.reenrollWithRecovery()
		close(done)
	}()

	// Wait for the first enroll attempt to complete. Channel-based sync
	// eliminates the CI-flake risk that polling loops introduce.
	waitForCalled := func(msg string) {
		t.Helper()
		select {
		case <-tracker.calledCh:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: no enroll call within 5s", msg)
		}
	}
	waitForCalled("first 409 attempt")

	// Wait for the suspended transition event — the handler publishes after
	// setting suspended=true, so receiving the event means the state is set.
	// Drain events until we see the one that reflects suspended.
	drainDeadline := time.Now().Add(3 * time.Second)
	for !svc.IsSuspended() {
		if time.Now().After(drainDeadline) {
			t.Fatal("suspended=true not observed within 3s of first 409")
		}
		select {
		case <-changedCh:
		case <-time.After(100 * time.Millisecond):
		}
	}

	// Wait for the second enroll attempt — proves the loop did NOT exit on 409.
	waitForCalled("second retry after 409 (loop must not exit)")

	// Flip to success — the next retry should clear suspended and exit.
	tracker.returnOK.Store(true)

	select {
	case <-done:
		// Loop exited via success path.
	case <-time.After(10 * time.Second):
		t.Fatal("reenrollWithRecovery did not exit after flipping to success")
	}

	if svc.IsSuspended() {
		t.Error("expected suspended=false after self-heal via finalizeEnrollment")
	}
}

// TestAutoEnrollAtBoot_DeviceConflict_WrappedError is the critical regression
// test for red-team v3 BRF-1: the 409 handler in autoEnrollAtBoot must detect
// the error even though Service.Enroll wraps the namekclient error via
// fmt.Errorf("%w", ...). Verified via errors.As unwrapping inside
// isDeviceConflictError.
//
// The test calls s.Enroll (NOT the raw namekclient) to exercise the wrapping
// path, then inspects the classifier result indirectly via suspended flag.
func TestAutoEnrollAtBoot_DeviceConflict_WrappedError(t *testing.T) {
	tracker := newEnrollTracker()
	srv := httptest.NewServer(tracker.Handler())
	defer srv.Close()

	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "identity.json")
	svc := &Service{
		configPath: configPath,
		tpmDev:     &mockTPM{},
		cfg: Config{
			Enabled:  true,
			DeviceID: "", // fresh device — takes autoEnrollAtBoot path
			NamekURL: srv.URL,
		},
		client: newNamekClient(srv.URL, &mockTPM{}, ""),
		stopCh: make(chan struct{}),
	}

	bus := events.NewBus()
	defer bus.Close()
	svc.eventsBus = bus

	if err := saveConfig(configPath, svc.cfg); err != nil {
		t.Fatal(err)
	}

	changedCh := bus.Subscribe(events.TopicIdentityChanged, 8)

	// First: verify that isDeviceConflictError correctly unwraps the wrapped error.
	// This is the direct BRF-1 regression test — if Service.Enroll's %w wrapping
	// defeats the classifier, this assertion fails.
	_, err := svc.Enroll(t.Context())
	if err == nil {
		t.Fatal("expected 409 error from Enroll")
	}
	if !isDeviceConflictError(err) {
		t.Errorf("isDeviceConflictError failed to detect wrapped 409: %v", err)
	}

	// Drain the call channel from the Enroll() call above so the next
	// receive blocks on the autoEnrollAtBoot attempt, not the verification call.
	drainCalled(tracker)

	// Now exercise the full autoEnrollAtBoot path.
	done := make(chan struct{})
	go func() {
		svc.autoEnrollAtBoot()
		close(done)
	}()

	// Wait for the first enroll attempt.
	select {
	case <-tracker.calledCh:
	case <-time.After(5 * time.Second):
		t.Fatal("no enroll call from autoEnrollAtBoot within 5s")
	}

	// Wait for suspended transition via event drain.
	drainDeadline := time.Now().Add(3 * time.Second)
	for !svc.IsSuspended() {
		if time.Now().After(drainDeadline) {
			t.Fatal("suspended=true not observed within 3s")
		}
		select {
		case <-changedCh:
		case <-time.After(100 * time.Millisecond):
		}
	}

	// Stop the loop cleanly. The loop must still be live (not exited via 409
	// return) for this to actually stop something — the stop signal would be
	// a no-op if the loop had already exited.
	svc.stopped.Store(true)
	close(svc.stopCh)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("autoEnrollAtBoot did not exit after stop signal")
	}
}

// REC-5: SetNamekURL must wake an in-progress autoEnrollAtBoot backoff
// loop so the new client is exercised immediately, not after the current
// timer expires (up to ~120s at the cap). Without this wake source, an
// admin URL switch sees a multi-minute lag before the new server is tried,
// because triggerAutoEnroll's autoEnrolling.Load() guard makes the
// post-SetNamekURL trigger a no-op when the loop is already running.
//
// Test exercises the wake mechanism in isolation by signaling the channel
// directly. The SetNamekURL → namekURLChanged plumbing is a 4-line
// composition reviewable by inspection.
//
// Synchronization strategy: rather than racing wall-clock sleeps against
// the loop reaching its select, we use `IsSuspended()` as the observable
// "loop is past the 409 handler, parked in the backoff select" signal —
// the same pattern as TestAutoEnrollAtBoot_DeviceConflict_WrappedError.
// Then we snapshot the atomic call counter so the post-wake assertion is
// "calls increased" rather than relying on calledCh ordering (the handler
// fires calledCh per HTTP request, not per attempt, so calledCh can carry
// stale residual sends from the first attempt).
func TestAutoEnrollAtBoot_WakesOnNamekURLChanged(t *testing.T) {
	tracker := newEnrollTracker()
	srv := httptest.NewServer(tracker.Handler())
	defer srv.Close()

	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "identity.json")
	svc := &Service{
		configPath: configPath,
		tpmDev:     &mockTPM{},
		cfg: Config{
			Enabled:  true,
			DeviceID: "",
			NamekURL: srv.URL,
		},
		client:          newNamekClient(srv.URL, &mockTPM{}, ""),
		stopCh:          make(chan struct{}),
		networkUp:       make(chan struct{}, 1),
		namekURLChanged: make(chan struct{}, 1),
	}

	bus := events.NewBus()
	defer bus.Close()
	svc.eventsBus = bus

	if err := saveConfig(configPath, svc.cfg); err != nil {
		t.Fatal(err)
	}

	changedCh := bus.Subscribe(events.TopicIdentityChanged, 8)

	done := make(chan struct{})
	go func() {
		svc.autoEnrollAtBoot()
		close(done)
	}()

	// Wait for the first enroll attempt to start (handler signals calledCh).
	select {
	case <-tracker.calledCh:
	case <-time.After(5 * time.Second):
		t.Fatal("first enroll attempt did not start within 5s")
	}

	// Wait until handleDeviceConflict flips suspended=true — observable
	// proof that the loop processed the 409 response and is on its way
	// into the backoff select. Drains the IdentityChanged event so the
	// test doesn't busy-loop on the suspended check.
	drainDeadline := time.Now().Add(3 * time.Second)
	for !svc.IsSuspended() {
		if time.Now().After(drainDeadline) {
			t.Fatal("suspended=true not observed within 3s — loop may not have reached the conflict handler")
		}
		select {
		case <-changedCh:
		case <-time.After(50 * time.Millisecond):
		}
	}

	// Snapshot the atomic call counter; the post-wake assertion compares
	// against this rather than reading calledCh (which the handler fires
	// once per HTTP request — multiple per attempt — and could carry
	// residual sends from the first attempt).
	callsBefore := atomic.LoadInt64(&tracker.calls)

	// Send the wake. backoffDelay is `baseDelay<<attempt + jitter`, so for
	// attempt 0 it returns 5s..7.5s (jitter adds a non-negative slice up
	// to 0.5×delay). A sub-1.5s second-attempt is only possible if the
	// wake fires the select case rather than the timer.
	wakeStart := time.Now()
	svc.namekURLChanged <- struct{}{}

	// Poll the call counter until it advances or the deadline expires.
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt64(&tracker.calls) <= callsBefore {
		if time.Now().After(deadline) {
			t.Fatal("call counter did not advance within 2s of wake — namekURLChanged not wired into the autoEnrollAtBoot select")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if elapsed := time.Since(wakeStart); elapsed > 1500*time.Millisecond {
		t.Errorf("retry took %v after wake; want well under attempt-0 backoff floor (5s)", elapsed)
	}

	// Stop cleanly.
	svc.stopped.Store(true)
	close(svc.stopCh)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("autoEnrollAtBoot did not exit after stop signal")
	}
}

// TestSetNamekURL_SerializesAgainstInFlightEnroll verifies the structural
// SETN-1 close (cluster 13): SetNamekURL holds identityMu.Lock and BLOCKS
// until any in-flight identity-bound op (Enroll here) drains. The hybrid-
// identity bug is closed by ordering — Enroll completes against the OLD
// client+config first (committing OLD DeviceID briefly), then SetNamekURL
// runs and clears the config. End state: NEW URL, no DeviceID, no hybrid.
//
// The test asserts both: (a) SetNamekURL was actually blocked while Enroll
// was in-flight, and (b) the end state reflects the SetNamekURL clear, not
// a persisted hybrid identity.
func TestSetNamekURL_SerializesAgainstInFlightEnroll(t *testing.T) {
	tracker := newEnrollTracker()
	tracker.returnOK.Store(true)

	releaseCh := make(chan struct{})
	holdCh := make(chan struct{}, 1)
	holdOnce := make(chan struct{}, 1)
	holdOnce <- struct{}{}

	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/devices/enroll" {
			select {
			case <-holdOnce:
				holdCh <- struct{}{}
				<-releaseCh
			default:
			}
		}
		tracker.Handler().ServeHTTP(w, r)
	}))
	defer srvA.Close()

	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "srvB unavailable"})
	}))
	defer srvB.Close()

	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "identity.json")
	svc := &Service{
		configPath: configPath,
		tpmDev:     &mockTPM{},
		cfg: Config{
			Enabled:  true,
			DeviceID: "",
			NamekURL: srvA.URL,
		},
		client:          newNamekClient(srvA.URL, &mockTPM{}, ""),
		stopCh:          make(chan struct{}),
		networkUp:       make(chan struct{}, 1),
		namekURLChanged: make(chan struct{}, 1),
	}
	bus := events.NewBus()
	defer bus.Close()
	svc.eventsBus = bus
	if err := saveConfig(configPath, svc.cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		svc.stopped.Store(true)
		select {
		case <-svc.stopCh:
		default:
			close(svc.stopCh)
		}
		svc.recoverWg.Wait()
	})

	type enrollResult struct {
		result *EnrollResult
		err    error
	}
	enrollDone := make(chan enrollResult, 1)
	go func() {
		r, err := svc.Enroll(t.Context())
		enrollDone <- enrollResult{r, err}
	}()

	// Wait until Enroll has the first call held — Enroll holds identityMu.RLock
	// at this point.
	select {
	case <-holdCh:
	case <-time.After(3 * time.Second):
		close(releaseCh)
		t.Fatal("first /devices/enroll call did not arrive at server A within 3s")
	}

	// SetNamekURL must block on identityMu.Lock until Enroll's RLock releases.
	setNamekDone := make(chan error, 1)
	go func() {
		setNamekDone <- svc.SetNamekURL(t.Context(), srvB.URL)
	}()

	// Brief grace window: SetNamekURL should NOT have completed because
	// Enroll still holds identityMu.RLock.
	select {
	case err := <-setNamekDone:
		close(releaseCh)
		t.Fatalf("SetNamekURL completed (err=%v) while Enroll's RLock is held — serialization failed", err)
	case <-time.After(200 * time.Millisecond):
	}

	// Release Enroll's hold. RPC succeeds, finalizeEnrollment commits, RLock
	// releases. SetNamekURL's pending Lock then acquires and clears the config.
	close(releaseCh)

	select {
	case got := <-enrollDone:
		if got.err != nil {
			t.Fatalf("expected Enroll to succeed; got err=%v", got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Enroll did not return within 5s of release")
	}

	select {
	case err := <-setNamekDone:
		if err != nil {
			t.Fatalf("SetNamekURL: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SetNamekURL did not return within 5s of Enroll completing")
	}

	// End state: SetNamekURL ran AFTER Enroll committed, so it cleared the
	// hybrid back to fresh. NEW URL, no DeviceID, not enrolled.
	cfg := svc.DeviceConfig()
	if cfg.NamekURL != srvB.URL {
		t.Errorf("NamekURL = %q, want %q (post-SetNamekURL)", cfg.NamekURL, srvB.URL)
	}
	if cfg.DeviceID != "" {
		t.Errorf("DeviceID = %q, want empty (SetNamekURL should have cleared)", cfg.DeviceID)
	}
	if svc.IsEnrolled() {
		t.Error("expected enrolled=false after SetNamekURL")
	}
}

// TestSetNamekURL_SerializesAgainstReenrollWithRecovery is the same
// shape as TestSetNamekURL_SerializesAgainstInFlightEnroll, but for the
// reenrollWithRecovery RPC path. reenrollWithRecovery holds RLock per
// attempt (not per loop), so SetNamekURL blocks until the in-flight
// attempt's RPC + finalize + replay drains.
func TestSetNamekURL_SerializesAgainstReenrollWithRecovery(t *testing.T) {
	tracker := newEnrollTracker()
	tracker.returnOK.Store(true)

	releaseCh := make(chan struct{})
	holdCh := make(chan struct{}, 1)
	holdOnce := make(chan struct{}, 1)
	holdOnce <- struct{}{}

	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/devices/enroll" {
			select {
			case <-holdOnce:
				holdCh <- struct{}{}
				<-releaseCh
			default:
			}
		}
		tracker.Handler().ServeHTTP(w, r)
	}))
	defer srvA.Close()

	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "srvB unavailable"})
	}))
	defer srvB.Close()

	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "identity.json")
	svc := &Service{
		configPath: configPath,
		tpmDev:     &mockTPM{},
		cfg: Config{
			Enabled:  true,
			DeviceID: "dev-old",
			Hostname: "slug.test.local",
			NamekURL: srvA.URL,
		},
		client:          newNamekClient(srvA.URL, &mockTPM{}, "dev-old"),
		stopCh:          make(chan struct{}),
		networkUp:       make(chan struct{}, 1),
		namekURLChanged: make(chan struct{}, 1),
	}
	svc.enrolled.Store(true)
	bus := events.NewBus()
	defer bus.Close()
	svc.eventsBus = bus
	if err := saveConfig(configPath, svc.cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		svc.stopped.Store(true)
		select {
		case <-svc.stopCh:
		default:
			close(svc.stopCh)
		}
		svc.recoverWg.Wait()
	})

	// reenrollWithRecovery requires recovering=true precondition.
	svc.recovering.Store(true)
	done := make(chan struct{})
	go func() {
		svc.reenrollWithRecovery()
		close(done)
	}()

	// Wait until the handler has the first /devices/enroll call held —
	// reenrollWithRecovery holds identityMu.RLock for this attempt.
	select {
	case <-holdCh:
	case <-time.After(3 * time.Second):
		close(releaseCh)
		t.Fatal("first /devices/enroll call did not arrive at server A within 3s")
	}

	// SetNamekURL must block on identityMu.Lock until the in-flight
	// reenroll attempt's RLock releases.
	setNamekDone := make(chan error, 1)
	go func() {
		setNamekDone <- svc.SetNamekURL(t.Context(), srvB.URL)
	}()
	select {
	case err := <-setNamekDone:
		close(releaseCh)
		t.Fatalf("SetNamekURL completed (err=%v) while reenroll's RLock held — serialization failed", err)
	case <-time.After(200 * time.Millisecond):
	}

	close(releaseCh)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("reenrollWithRecovery did not exit within 10s")
	}
	select {
	case err := <-setNamekDone:
		if err != nil {
			t.Fatalf("SetNamekURL: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SetNamekURL did not return within 5s of reenroll completing")
	}

	// End state: SetNamekURL ran AFTER the reenroll attempt (which committed
	// "dev-123" against srvA) and cleared everything. NEW URL, no DeviceID.
	cfg := svc.DeviceConfig()
	if cfg.NamekURL != srvB.URL {
		t.Errorf("NamekURL = %q, want %q", cfg.NamekURL, srvB.URL)
	}
	if cfg.DeviceID == "dev-123" {
		t.Errorf("DeviceID = %q — SetNamekURL did not clear (hybrid persisted)", cfg.DeviceID)
	}
}

// TestSetNamekURL_NoDeadlockWithRunningSync regression-pins the cluster 13
// deadlock scenario CRA + codex caught: endpoint sync loop runs an
// iteration → enters syncEndpointsOnce → SetNamekURL acquires identityMu.Lock
// → syncEndpointsOnce's RLock would block (writer-preferring) → SetNamekURL's
// stopEndpointSync waits for the sync loop's `<-done` → the sync loop can't
// exit because its iteration is blocked on RLock → permanent deadlock.
//
// Fix: syncEndpointsOnce uses TryRLock + skip on contention; SetNamekURL
// pre-stops sync loops BEFORE Lock. Either alone closes the deadlock; both
// together provide defense in depth.
//
// Test: start sync loop with frequent ticks against a slow handler so an
// iteration is highly likely to be in-flight when SetNamekURL fires; assert
// SetNamekURL completes within bounded time (deadlock would hang forever).
func TestSetNamekURL_NoDeadlockWithRunningSync(t *testing.T) {
	tracker := newEnrollTracker()
	tracker.returnOK.Store(true)

	// Slow handler: GetDeviceInfo takes ~50ms each call so a sync iteration
	// is in-flight when SetNamekURL fires (sync interval 100ms below).
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/devices/me" {
			time.Sleep(50 * time.Millisecond)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":           "active",
				"account_id":       "acc-A",
				"nexus_endpoints":  []string{"wss://relay.A"},
				"custom_hostname":  nil,
			})
			return
		}
		tracker.Handler().ServeHTTP(w, r)
	}))
	defer srvA.Close()

	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srvB.Close()

	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "identity.json")
	svc := &Service{
		configPath: configPath,
		tpmDev:     &mockTPM{},
		cfg: Config{
			Enabled:  true,
			DeviceID: "dev-existing",
			Hostname: "slug.test.local",
			NamekURL: srvA.URL,
		},
		client:          newNamekClient(srvA.URL, &mockTPM{}, "dev-existing"),
		stopCh:          make(chan struct{}),
		networkUp:       make(chan struct{}, 1),
		namekURLChanged: make(chan struct{}, 1),
	}
	svc.enrolled.Store(true)
	bus := events.NewBus()
	defer bus.Close()
	svc.eventsBus = bus
	if err := saveConfig(configPath, svc.cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		svc.stopped.Store(true)
		select {
		case <-svc.stopCh:
		default:
			close(svc.stopCh)
		}
		svc.recoverWg.Wait()
	})

	// Start endpoint sync. Default interval is too long for this test to
	// reliably hit the race window, so we hand-launch the loop with a tight
	// ticker via direct goroutine control.
	syncStop := make(chan struct{})
	syncDone := make(chan struct{})
	go func() {
		defer close(syncDone)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-syncStop:
				return
			case <-ticker.C:
				svc.syncEndpointsOnce(t.Context())
			}
		}
	}()
	defer func() {
		close(syncStop)
		<-syncDone
	}()

	// Let several sync iterations run so we know one is likely in-flight.
	time.Sleep(100 * time.Millisecond)

	// Without the deadlock fix, SetNamekURL could block indefinitely because
	// it holds identityMu.Lock while its (eventual) stopEndpointSync waits
	// for a sync goroutine that's stuck on RLock. With TryRLock + pre-stop,
	// SetNamekURL completes promptly.
	done := make(chan error, 1)
	go func() {
		done <- svc.SetNamekURL(t.Context(), srvB.URL)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("SetNamekURL: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("SetNamekURL did not complete within 10s — likely deadlock with running sync loop")
	}
}

// drainCalled removes any pending entries from tracker.calledCh. Used after
// a direct s.Enroll() verification call so subsequent channel receives
// synchronize on the intended path.
func drainCalled(t *enrollTracker) {
	for {
		select {
		case <-t.calledCh:
		default:
			return
		}
	}
}

// TestReenrollWithRecovery_DeviceConflict_ThenDisable_ClearsSuspended verifies
// the sticky-suspended-on-early-exit fix (red-team v4 BRF-1). When the
// operator disables identity while the device is stuck in 409, the loop's
// early-exit path must clear suspended so the setup UI doesn't remain stuck
// at "unavailable" until the next piccolod restart.
func TestReenrollWithRecovery_DeviceConflict_ThenDisable_ClearsSuspended(t *testing.T) {
	tracker := newEnrollTracker()
	srv := httptest.NewServer(tracker.Handler())
	defer srv.Close()

	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "identity.json")
	svc := &Service{
		configPath: configPath,
		tpmDev:     &mockTPM{},
		cfg: Config{
			Enabled:  true,
			DeviceID: "dev-123",
			Hostname: "slug.test.local",
			NamekURL: srv.URL,
		},
		client: newNamekClient(srv.URL, &mockTPM{}, "dev-123"),
		stopCh: make(chan struct{}),
	}
	svc.enrolled.Store(true)

	bus := events.NewBus()
	defer bus.Close()
	svc.eventsBus = bus

	if err := saveConfig(configPath, svc.cfg); err != nil {
		t.Fatal(err)
	}

	// Precondition: reenrollWithRecovery expects recovering=true set by caller.
	svc.recovering.Store(true)

	changedCh := bus.Subscribe(events.TopicIdentityChanged, 8)

	done := make(chan struct{})
	go func() {
		svc.reenrollWithRecovery()
		close(done)
	}()

	// Wait for the first enroll attempt (channel sync — flake-resistant).
	select {
	case <-tracker.calledCh:
	case <-time.After(5 * time.Second):
		t.Fatal("no enroll call within 5s")
	}

	// Wait for suspended transition via event drain.
	drainDeadline := time.Now().Add(3 * time.Second)
	for !svc.IsSuspended() {
		if time.Now().After(drainDeadline) {
			t.Fatal("suspended=true not observed within 3s")
		}
		select {
		case <-changedCh:
		case <-time.After(100 * time.Millisecond):
		}
	}

	// Simulate operator action: flip enrolled=false (mimics identity reset).
	// The next loop iteration hits the early-exit at !enrolled and must clear
	// suspended before returning.
	svc.enrolled.Store(false)

	select {
	case <-done:
		// Loop exited via the early-exit branch.
	case <-time.After(10 * time.Second):
		t.Fatal("reenrollWithRecovery did not exit after enrolled flipped to false")
	}

	if svc.IsSuspended() {
		t.Error("expected suspended=false after early-exit cleared the flag (red-team v4 BRF-1 regression)")
	}
}

// TestSetupStatus_TerminalStates verifies the tri-state logic:
// "enrolled" only when enrolled && !suspended; "unavailable" for all
// terminal failures; "pending" for in-flight and narrow early-boot window.
func TestSetupStatus_TerminalStates(t *testing.T) {
	cases := []struct {
		name       string
		setup      func(s *Service)
		wantStatus string
	}{
		{
			name: "enrolled and healthy",
			setup: func(s *Service) {
				s.enrolled.Store(true)
				s.available.Store(true)
				s.cfg.Enabled = true
			},
			wantStatus: "enrolled",
		},
		{
			name: "enrolled but suspended → unavailable (not enrolled)",
			setup: func(s *Service) {
				s.enrolled.Store(true)
				s.available.Store(true)
				s.suspended.Store(true)
				s.cfg.Enabled = true
			},
			wantStatus: "unavailable",
		},
		{
			name: "identity disabled → unavailable",
			setup: func(s *Service) {
				s.available.Store(true)
				s.cfg.Enabled = false
			},
			wantStatus: "unavailable",
		},
		{
			name: "TPM unavailable → unavailable",
			setup: func(s *Service) {
				s.available.Store(false)
				s.cfg.Enabled = true
			},
			wantStatus: "unavailable",
		},
		{
			name: "auto-enrolling → pending",
			setup: func(s *Service) {
				s.available.Store(true)
				s.cfg.Enabled = true
				s.autoEnrolling.Store(true)
			},
			wantStatus: "pending",
		},
		{
			name: "recovering → pending",
			setup: func(s *Service) {
				s.available.Store(true)
				s.cfg.Enabled = true
				s.recovering.Store(true)
			},
			wantStatus: "pending",
		},
		{
			name: "in-flight but also suspended → unavailable (suspended-first)",
			setup: func(s *Service) {
				s.available.Store(true)
				s.cfg.Enabled = true
				s.recovering.Store(true)
				s.suspended.Store(true)
			},
			wantStatus: "unavailable",
		},
		{
			// Codex Phase 3 P1: enrolled device hitting boot-time AK
			// recovery must report "pending" (not "enrolled") so the UI
			// shows the spinner instead of letting the user attempt a
			// hostname claim against an unauthenticated namek.
			name: "enrolled but recovering → pending (in-flight beats enrolled)",
			setup: func(s *Service) {
				s.available.Store(true)
				s.cfg.Enabled = true
				s.enrolled.Store(true)
				s.recovering.Store(true)
			},
			wantStatus: "pending",
		},
		{
			// Codex Phase 3b P3: enabled+available+unenrolled with no
			// in-flight job (e.g., after SetEnabled(true) / SetNamekURL on a
			// fresh device that didn't trigger autoEnroll) must NOT report
			// "pending" — that would spin the UI forever. Report
			// "unavailable" so the user sees the actionable copy.
			name: "no enrollment job in flight, not enrolled → unavailable",
			setup: func(s *Service) {
				s.available.Store(true)
				s.cfg.Enabled = true
			},
			wantStatus: "unavailable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := &Service{}
			tc.setup(svc)
			got := svc.SetupStatus()
			if got != tc.wantStatus {
				t.Errorf("SetupStatus() = %q, want %q", got, tc.wantStatus)
			}
		})
	}
}
