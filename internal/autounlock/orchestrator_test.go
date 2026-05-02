package autounlock

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sync"
	"testing"
	"time"

	"piccolod/internal/crypt"
	"piccolod/internal/state/paths"

	"github.com/AtDexters-Lab/namek-server/pkg/namekclient"
)

// fakeManager implements ManagerOps. Backed by a real crypt.Manager when the
// test needs SDEK semantics; bypassed when we just need IsLocked / Wrap /
// Unwrap stubs for failure-path tests.
type fakeManager struct {
	mu              sync.Mutex
	locked          bool
	sdek            []byte
	wrapErr         error
	unwrapErr       error
	wrapCallCount   int
	unwrapCallCount int
	// recorded args for assertion
	lastWrapAAD   []byte
	lastUnwrapF   []byte
	lastUnwrapAAD []byte
}

func (f *fakeManager) IsLocked() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.locked
}

func (f *fakeManager) WrapSDEKForEscrow(F []byte, aad []byte) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wrapCallCount++
	f.lastWrapAAD = append([]byte(nil), aad...)
	if f.wrapErr != nil {
		return nil, f.wrapErr
	}
	// Return F + sdek concatenated so unwrap can verify both.
	return append(append([]byte(nil), F...), f.sdek...), nil
}

func (f *fakeManager) UnwrapSDEKWithEscrow(blob []byte, F []byte, aad []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unwrapCallCount++
	f.lastUnwrapF = append([]byte(nil), F...)
	f.lastUnwrapAAD = append([]byte(nil), aad...)
	return f.unwrapErr
}

// fakeNamek implements NamekEscrowClient. Tracks call counts and lets tests
// inject errors per method. depositErrs / pickupErrs are popped per call so
// tests can simulate "transient on first attempt, success on retry" cleanly.
type fakeNamek struct {
	mu             sync.Mutex
	depositErrs    []error // popped per call; remainder defaults to nil
	pickupErrs     []error // popped per call; remainder defaults to nil
	revokeErr      error
	depositResp    *namekclient.DepositUnlockEscrowResponse
	pickupResp     *namekclient.PickupUnlockEscrowResponse
	pickupEchoesF  bool // when true, pickup returns base64(lastDepositF)
	depositCount   int
	pickupCount    int
	revokeCount    int
	lastDepositF   []byte
	lastDepositWin int
}

func (f *fakeNamek) popErr(slice *[]error) error {
	if len(*slice) == 0 {
		return nil
	}
	err := (*slice)[0]
	*slice = (*slice)[1:]
	return err
}

func (f *fakeNamek) DepositUnlockEscrow(ctx context.Context, secret []byte, windowSeconds int) (*namekclient.DepositUnlockEscrowResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.depositCount++
	f.lastDepositF = append([]byte(nil), secret...)
	f.lastDepositWin = windowSeconds
	if err := f.popErr(&f.depositErrs); err != nil {
		return nil, err
	}
	if f.depositResp != nil {
		return f.depositResp, nil
	}
	return &namekclient.DepositUnlockEscrowResponse{
		ExpiresAt:              "2026-05-02T00:00:00Z",
		EffectiveWindowSeconds: windowSeconds,
		RequestedClamped:       false,
	}, nil
}

func (f *fakeNamek) PickupUnlockEscrow(ctx context.Context) (*namekclient.PickupUnlockEscrowResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pickupCount++
	if err := f.popErr(&f.pickupErrs); err != nil {
		return nil, err
	}
	if f.pickupEchoesF {
		return &namekclient.PickupUnlockEscrowResponse{
			Secret:    base64.RawURLEncoding.EncodeToString(f.lastDepositF),
			ExpiresAt: "2026-05-02T00:10:00Z",
		}, nil
	}
	return f.pickupResp, nil
}

func (f *fakeNamek) RevokeUnlockEscrow(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revokeCount++
	return f.revokeErr
}

type capturedAudit struct {
	kind    string
	details map[string]any
}

func newOrchestrator(t *testing.T) (*Orchestrator, *fakeManager, *fakeNamek, *[]capturedAudit) {
	t.Helper()
	paths.SetCoreRootForTest(t, t.TempDir())
	mgr := &fakeManager{sdek: []byte("test-sdek-32-bytes-padded-padded")}
	nc := &fakeNamek{}
	audits := &[]capturedAudit{}
	auditMu := sync.Mutex{}
	deps := Deps{
		Manager:          mgr,
		NamekClient:      func() NamekEscrowClient { return nc },
		GetDeviceID:      func() string { return "dev-test-1" },
		GetIdentityClass: func() string { return IdentityClassHardwareTPM },
		IsIdentityReady:  func() bool { return true },
		PublishAudit: func(kind string, details map[string]any) {
			auditMu.Lock()
			defer auditMu.Unlock()
			*audits = append(*audits, capturedAudit{kind, details})
		},
		Now: func() time.Time { return time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC) },
	}
	o, err := New(deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return o, mgr, nc, audits
}

// --- ceremony ---

func TestRunCeremony_PostureOff_NoOp(t *testing.T) {
	o, _, nc, audits := newOrchestrator(t)
	if err := SaveState(State{Posture: PostureOff}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	if err := o.RunCeremony(context.Background()); err != nil {
		t.Fatalf("RunCeremony: %v", err)
	}
	if nc.depositCount != 0 {
		t.Errorf("deposit called on posture=off; count=%d", nc.depositCount)
	}
	if BlobExists() {
		t.Errorf("blob written on posture=off")
	}
	if len(*audits) != 0 {
		t.Errorf("audit emitted on posture=off; count=%d", len(*audits))
	}
}

func TestRunCeremony_HappyPath(t *testing.T) {
	o, mgr, nc, audits := newOrchestrator(t)
	if err := SaveState(State{Posture: PostureTPMAuto}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	if err := o.RunCeremony(context.Background()); err != nil {
		t.Fatalf("RunCeremony: %v", err)
	}
	if mgr.wrapCallCount != 1 {
		t.Errorf("wrap call count = %d; want 1", mgr.wrapCallCount)
	}
	if nc.depositCount != 1 {
		t.Errorf("deposit count = %d; want 1", nc.depositCount)
	}
	if !BlobExists() {
		t.Errorf("blob not written after happy ceremony")
	}
	// AAD must contain device id + suffix.
	wantAAD := []byte("dev-test-1|" + aadSuffix)
	if string(mgr.lastWrapAAD) != string(wantAAD) {
		t.Errorf("AAD = %q; want %q", mgr.lastWrapAAD, wantAAD)
	}
	// Window must be 600s (the production setting).
	if nc.lastDepositWin != configuredWindowSeconds {
		t.Errorf("window = %d; want %d", nc.lastDepositWin, configuredWindowSeconds)
	}
	if len(*audits) != 1 || (*audits)[0].kind != AuditCycleDeposited {
		t.Errorf("expected 1 deposited audit; got %v", *audits)
	}
}

func TestRunCeremony_PostureNotEligible_NoOp(t *testing.T) {
	o, _, nc, _ := newOrchestrator(t)
	if err := SaveState(State{Posture: PostureSoftAuto}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	// identity_class is hardware_tpm — soft_auto is ineligible.
	if err := o.RunCeremony(context.Background()); err != nil {
		t.Fatalf("RunCeremony: %v", err)
	}
	if nc.depositCount != 0 {
		t.Errorf("deposit called when posture ineligible; count=%d", nc.depositCount)
	}
}

func TestRunCeremony_ManagerLocked_NoOp(t *testing.T) {
	o, mgr, nc, _ := newOrchestrator(t)
	mgr.locked = true
	if err := SaveState(State{Posture: PostureTPMAuto}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	if err := o.RunCeremony(context.Background()); err != nil {
		t.Fatalf("RunCeremony: %v", err)
	}
	if nc.depositCount != 0 {
		t.Errorf("deposit called with manager locked; count=%d", nc.depositCount)
	}
	if mgr.wrapCallCount != 0 {
		t.Errorf("wrap called with manager locked; count=%d", mgr.wrapCallCount)
	}
}

func TestRunCeremony_DepositFails_DeletesBlob(t *testing.T) {
	o, _, nc, audits := newOrchestrator(t)
	if err := SaveState(State{Posture: PostureTPMAuto}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	// Both attempts fail (transport error → retried once, retry also fails).
	nc.depositErrs = []error{errors.New("namek down"), errors.New("namek down")}
	err := o.RunCeremony(context.Background())
	if err == nil {
		t.Fatalf("expected error from RunCeremony")
	}
	if BlobExists() {
		t.Errorf("blob retained after deposit failure")
	}
	state, _ := LoadState()
	if state.LastFailureReason != ReasonDepositFailed {
		t.Errorf("last_failure_reason = %q; want %q", state.LastFailureReason, ReasonDepositFailed)
	}
	if len(*audits) != 1 || (*audits)[0].kind != AuditCycleFailedPickup {
		t.Errorf("expected failed_pickup audit; got %v", *audits)
	}
}

func TestRunCeremony_DepositRetriesOnTransient(t *testing.T) {
	o, _, nc, _ := newOrchestrator(t)
	if err := SaveState(State{Posture: PostureTPMAuto}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	// First attempt: 503 transient. Second attempt: success (no error queued).
	nc.depositErrs = []error{&namekclient.APIError{StatusCode: 503, Message: "Service Unavailable"}}
	if err := o.RunCeremony(context.Background()); err != nil {
		t.Fatalf("RunCeremony with transient retry: %v", err)
	}
	if nc.depositCount != 2 {
		t.Errorf("expected exactly 2 deposit attempts; got %d", nc.depositCount)
	}
}

func TestRunCeremony_FRetryStable(t *testing.T) {
	// Verify the deposit retry reuses the same F bytes — load-bearing for
	// the blob/F desync invariant from Red-team F1.
	o, _, nc, _ := newOrchestrator(t)
	if err := SaveState(State{Posture: PostureTPMAuto}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	// First attempt fails (transport-level → retryable); second succeeds.
	nc.depositErrs = []error{errors.New("transient transport")}

	// Wrap the namek client so we can capture each call's F separately.
	var attempts [][]byte
	wrapped := &capturingNamek{inner: nc, onDeposit: func(F []byte) {
		attempts = append(attempts, append([]byte(nil), F...))
	}}
	o.deps.NamekClient = func() NamekEscrowClient { return wrapped }

	if err := o.RunCeremony(context.Background()); err != nil {
		t.Fatalf("RunCeremony: %v", err)
	}
	if len(attempts) != 2 {
		t.Fatalf("expected 2 deposit attempts; got %d", len(attempts))
	}
	if !bytesEq(attempts[0], attempts[1]) {
		t.Errorf("F changed across retry: first=%x second=%x", attempts[0], attempts[1])
	}
}

// capturingNamek wraps fakeNamek to invoke a callback with each deposit's F
// before delegating. Used in F-retry-stable test where we need per-call
// observation, not just last-call.
type capturingNamek struct {
	inner     *fakeNamek
	onDeposit func(F []byte)
}

func (c *capturingNamek) DepositUnlockEscrow(ctx context.Context, secret []byte, windowSeconds int) (*namekclient.DepositUnlockEscrowResponse, error) {
	if c.onDeposit != nil {
		c.onDeposit(secret)
	}
	return c.inner.DepositUnlockEscrow(ctx, secret, windowSeconds)
}
func (c *capturingNamek) PickupUnlockEscrow(ctx context.Context) (*namekclient.PickupUnlockEscrowResponse, error) {
	return c.inner.PickupUnlockEscrow(ctx)
}
func (c *capturingNamek) RevokeUnlockEscrow(ctx context.Context) error {
	return c.inner.RevokeUnlockEscrow(ctx)
}

// --- pickup ---

func TestRunPickup_PostureOff_NoBlob(t *testing.T) {
	o, _, nc, _ := newOrchestrator(t)
	if err := SaveState(State{Posture: PostureOff}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	out := o.RunPickup(context.Background(), neverCalledChain(t))
	if out != PickupOutcomeNoBlob {
		t.Errorf("outcome = %v; want NoBlob", out)
	}
	if nc.pickupCount != 0 {
		t.Errorf("pickup called with posture=off; count=%d", nc.pickupCount)
	}
}

func TestRunPickup_NoBlobOnDisk_AuditsAndExits(t *testing.T) {
	o, _, _, audits := newOrchestrator(t)
	if err := SaveState(State{Posture: PostureTPMAuto}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	// No blob written.
	out := o.RunPickup(context.Background(), neverCalledChain(t))
	if out != PickupOutcomeNoBlob {
		t.Errorf("outcome = %v; want NoBlob", out)
	}
	if len(*audits) != 1 || (*audits)[0].kind != AuditCycleFailedPickup {
		t.Errorf("expected failed_pickup audit; got %v", *audits)
	}
	if (*audits)[0].details["reason"] != ReasonNoBlob {
		t.Errorf("reason = %v; want %s", (*audits)[0].details["reason"], ReasonNoBlob)
	}
}

func TestRunPickup_HappyPath(t *testing.T) {
	o, mgr, nc, audits := newOrchestrator(t)
	if err := SaveState(State{Posture: PostureTPMAuto}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	if err := WriteBlob([]byte("opaque-blob")); err != nil {
		t.Fatalf("WriteBlob: %v", err)
	}
	F := make([]byte, fSize)
	rand.Read(F)
	nc.pickupResp = &namekclient.PickupUnlockEscrowResponse{
		Secret:    base64.RawURLEncoding.EncodeToString(F),
		ExpiresAt: "2026-05-02T00:10:00Z",
	}
	chainCalled := false
	chain := func(ctx context.Context) error { chainCalled = true; return nil }
	out := o.RunPickup(context.Background(), chain)
	if out != PickupOutcomeUnlocked {
		t.Errorf("outcome = %v; want Unlocked", out)
	}
	if !chainCalled {
		t.Errorf("post-decrypt chain not called on success")
	}
	if mgr.unwrapCallCount != 1 {
		t.Errorf("unwrap call count = %d; want 1", mgr.unwrapCallCount)
	}
	if nc.revokeCount != 1 {
		t.Errorf("revoke count = %d; want 1", nc.revokeCount)
	}
	if BlobExists() {
		t.Errorf("blob retained after successful pickup")
	}
	// AAD passed to unwrap matches the orchestrator's aad().
	wantAAD := []byte("dev-test-1|" + aadSuffix)
	if string(mgr.lastUnwrapAAD) != string(wantAAD) {
		t.Errorf("unwrap AAD = %q; want %q", mgr.lastUnwrapAAD, wantAAD)
	}
	// Audit emitted.
	if len(*audits) != 1 || (*audits)[0].kind != AuditCyclePickedUp {
		t.Errorf("expected picked_up audit; got %v", *audits)
	}
}

func TestRunPickup_EscrowNotFound_DeletesBlob(t *testing.T) {
	o, _, nc, audits := newOrchestrator(t)
	if err := SaveState(State{Posture: PostureTPMAuto}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	if err := WriteBlob([]byte("opaque-blob")); err != nil {
		t.Fatalf("WriteBlob: %v", err)
	}
	nc.pickupErrs = []error{namekclient.ErrEscrowNotFound}
	out := o.RunPickup(context.Background(), neverCalledChain(t))
	if out != PickupOutcomeFailed {
		t.Errorf("outcome = %v; want Failed", out)
	}
	if BlobExists() {
		t.Errorf("blob retained after escrow-not-found")
	}
	if len(*audits) != 1 || (*audits)[0].details["reason"] != ReasonEscrowNotFound {
		t.Errorf("expected reason=escrow_not_found; got %v", *audits)
	}
}

func TestRunPickup_AuthFailed_RetainsBlob(t *testing.T) {
	o, _, nc, audits := newOrchestrator(t)
	if err := SaveState(State{Posture: PostureTPMAuto}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	if err := WriteBlob([]byte("opaque-blob")); err != nil {
		t.Fatalf("WriteBlob: %v", err)
	}
	nc.pickupErrs = []error{&namekclient.APIError{StatusCode: 401, Message: "Unauthorized"}}
	out := o.RunPickup(context.Background(), neverCalledChain(t))
	if out != PickupOutcomeFailed {
		t.Errorf("outcome = %v; want Failed", out)
	}
	if !BlobExists() {
		t.Errorf("blob deleted on auth_failed (should retain for retry)")
	}
	if (*audits)[0].details["reason"] != ReasonAuthFailed {
		t.Errorf("reason = %v; want %s", (*audits)[0].details["reason"], ReasonAuthFailed)
	}
}

func TestRunPickup_ManualUnlockFirst(t *testing.T) {
	o, mgr, nc, audits := newOrchestrator(t)
	if err := SaveState(State{Posture: PostureTPMAuto}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	if err := WriteBlob([]byte("opaque-blob")); err != nil {
		t.Fatalf("WriteBlob: %v", err)
	}
	F := make([]byte, fSize)
	rand.Read(F)
	nc.pickupResp = &namekclient.PickupUnlockEscrowResponse{
		Secret: base64.RawURLEncoding.EncodeToString(F),
	}
	mgr.unwrapErr = crypt.ErrAutoUnlockAlreadyUnlocked
	out := o.RunPickup(context.Background(), neverCalledChain(t))
	if out != PickupOutcomeManualUnlockFirst {
		t.Errorf("outcome = %v; want ManualUnlockFirst", out)
	}
	if nc.revokeCount != 1 {
		t.Errorf("revoke not called on manual_unlock_first race")
	}
	if BlobExists() {
		t.Errorf("blob retained after manual_unlock_first")
	}
	if (*audits)[0].kind != AuditCycleRevoked || (*audits)[0].details["reason"] != ReasonManualUnlockFirst {
		t.Errorf("expected revoked/manual_unlock_first audit; got %v", *audits)
	}
}

func TestRunPickup_BlobCorrupt_DeletesBlob(t *testing.T) {
	o, mgr, nc, audits := newOrchestrator(t)
	if err := SaveState(State{Posture: PostureTPMAuto}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	if err := WriteBlob([]byte("opaque-blob")); err != nil {
		t.Fatalf("WriteBlob: %v", err)
	}
	F := make([]byte, fSize)
	rand.Read(F)
	nc.pickupResp = &namekclient.PickupUnlockEscrowResponse{
		Secret: base64.RawURLEncoding.EncodeToString(F),
	}
	mgr.unwrapErr = crypt.ErrAutoUnlockBlobCorrupt
	out := o.RunPickup(context.Background(), neverCalledChain(t))
	if out != PickupOutcomeFailed {
		t.Errorf("outcome = %v; want Failed", out)
	}
	if BlobExists() {
		t.Errorf("corrupt blob retained")
	}
	if (*audits)[0].details["reason"] != ReasonBlobCorrupt {
		t.Errorf("reason = %v; want %s", (*audits)[0].details["reason"], ReasonBlobCorrupt)
	}
}

func TestRunPickup_ChainFails(t *testing.T) {
	o, _, nc, audits := newOrchestrator(t)
	if err := SaveState(State{Posture: PostureTPMAuto}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	if err := WriteBlob([]byte("opaque-blob")); err != nil {
		t.Fatalf("WriteBlob: %v", err)
	}
	F := make([]byte, fSize)
	rand.Read(F)
	nc.pickupResp = &namekclient.PickupUnlockEscrowResponse{
		Secret: base64.RawURLEncoding.EncodeToString(F),
	}
	chain := func(ctx context.Context) error { return errors.New("storage broken") }
	out := o.RunPickup(context.Background(), chain)
	if out != PickupOutcomeFailed {
		t.Errorf("outcome = %v; want Failed", out)
	}
	if (*audits)[0].details["reason"] != ReasonChainFailed {
		t.Errorf("reason = %v; want %s (chain failure)", (*audits)[0].details["reason"], ReasonChainFailed)
	}
}

// --- test action ---

func TestRunTest_HappyPath(t *testing.T) {
	o, _, nc, audits := newOrchestrator(t)
	if err := SaveState(State{Posture: PostureTPMAuto}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	// Echo the deposited F back on pickup so the test's byte-equality check passes.
	nc.pickupEchoesF = true

	res, err := o.RunTest(context.Background())
	if err != nil {
		t.Fatalf("RunTest: %v", err)
	}
	if !res.Success {
		t.Errorf("Success = false; ErrorKind = %q", res.ErrorKind)
	}
	if nc.depositCount != 1 || nc.pickupCount != 1 || nc.revokeCount != 1 {
		t.Errorf("deposit/pickup/revoke counts = %d/%d/%d; want 1/1/1",
			nc.depositCount, nc.pickupCount, nc.revokeCount)
	}
	// Window passed to deposit should be the small test window.
	if nc.lastDepositWin != testWindowSeconds {
		t.Errorf("test window = %d; want %d", nc.lastDepositWin, testWindowSeconds)
	}
	if len(*audits) != 1 || (*audits)[0].kind != AuditTestRun || (*audits)[0].details["success"] != true {
		t.Errorf("expected test.run success audit; got %v", *audits)
	}
}

func TestRunTest_PostureOff_PreconditionFails(t *testing.T) {
	o, _, nc, _ := newOrchestrator(t)
	if err := SaveState(State{Posture: PostureOff}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	_, err := o.RunTest(context.Background())
	if !errors.Is(err, ErrTestPreconditions) {
		t.Errorf("expected ErrTestPreconditions; got %v", err)
	}
	if nc.depositCount != 0 {
		t.Errorf("deposit called on posture=off; count=%d", nc.depositCount)
	}
}

func TestRunTest_DepositFails(t *testing.T) {
	o, _, nc, audits := newOrchestrator(t)
	if err := SaveState(State{Posture: PostureTPMAuto}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	nc.depositErrs = []error{&namekclient.APIError{StatusCode: 401, Message: "Unauthorized"}}
	res, err := o.RunTest(context.Background())
	if err != nil {
		t.Fatalf("RunTest: %v", err)
	}
	if res.Success {
		t.Errorf("expected failure")
	}
	if res.ErrorKind != ReasonAuthFailed {
		t.Errorf("ErrorKind = %q; want %q", res.ErrorKind, ReasonAuthFailed)
	}
	if (*audits)[0].details["error_kind"] != ReasonAuthFailed {
		t.Errorf("audit error_kind = %v; want %s", (*audits)[0].details["error_kind"], ReasonAuthFailed)
	}
}

// --- coverage gaps surfaced by review ---

func TestRunCeremony_WrapFails_RecordsDepositFailed(t *testing.T) {
	o, mgr, nc, audits := newOrchestrator(t)
	if err := SaveState(State{Posture: PostureTPMAuto}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	mgr.wrapErr = errors.New("manager refused wrap")
	if err := o.RunCeremony(context.Background()); err == nil {
		t.Fatalf("expected error from wrap failure")
	}
	if nc.depositCount != 0 {
		t.Errorf("deposit called after wrap failed; count=%d", nc.depositCount)
	}
	if BlobExists() {
		t.Errorf("blob written after wrap failed")
	}
	state, _ := LoadState()
	if state.LastFailureReason != ReasonDepositFailed {
		t.Errorf("reason = %q; want %q", state.LastFailureReason, ReasonDepositFailed)
	}
	if len(*audits) != 1 || (*audits)[0].kind != AuditCycleFailedPickup {
		t.Errorf("expected failed_pickup audit; got %v", *audits)
	}
}

func TestRunPickup_IdentityNotReady_RecordsServiceNotReady(t *testing.T) {
	o, _, nc, audits := newOrchestrator(t)
	if err := SaveState(State{Posture: PostureTPMAuto}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	if err := WriteBlob([]byte("opaque-blob")); err != nil {
		t.Fatalf("WriteBlob: %v", err)
	}
	o.deps.IsIdentityReady = func() bool { return false }
	out := o.RunPickup(context.Background(), neverCalledChain(t))
	if out != PickupOutcomeFailed {
		t.Errorf("outcome = %v; want Failed", out)
	}
	if nc.pickupCount != 0 {
		t.Errorf("pickup called when identity not ready; count=%d", nc.pickupCount)
	}
	if (*audits)[0].details["reason"] != ReasonServiceNotReady {
		t.Errorf("reason = %v; want %s", (*audits)[0].details["reason"], ReasonServiceNotReady)
	}
}

func TestRunTest_RevokeFails(t *testing.T) {
	o, _, nc, audits := newOrchestrator(t)
	if err := SaveState(State{Posture: PostureTPMAuto}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	nc.pickupEchoesF = true
	nc.revokeErr = &namekclient.APIError{StatusCode: 503, Message: "Service Unavailable"}
	res, err := o.RunTest(context.Background())
	if err != nil {
		t.Fatalf("RunTest: %v", err)
	}
	if res.Success {
		t.Errorf("expected test failure when revoke fails")
	}
	if res.ErrorKind != ReasonServiceUnreachable {
		t.Errorf("ErrorKind = %q; want %q", res.ErrorKind, ReasonServiceUnreachable)
	}
	if (*audits)[0].details["error_kind"] != ReasonServiceUnreachable {
		t.Errorf("audit error_kind = %v; want %s", (*audits)[0].details["error_kind"], ReasonServiceUnreachable)
	}
}

// --- helpers ---

func neverCalledChain(t *testing.T) CompleteUnlockChain {
	t.Helper()
	return func(ctx context.Context) error {
		t.Errorf("post-decrypt chain called when it should not be")
		return nil
	}
}

func bytesEq(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
