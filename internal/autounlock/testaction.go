package autounlock

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"time"

	"piccolod/internal/cryptoutil"

	"github.com/AtDexters-Lab/namek-server/pkg/namekclient"
)

// TestResult is returned by RunTest. ErrorKind is one of the failure-reason
// tokens (`service_unreachable`, `auth_failed`, etc.) when Success is false;
// empty otherwise. LatencyMs measures the full deposit + pickup + revoke
// round-trip.
type TestResult struct {
	Success   bool
	ErrorKind string
	LatencyMs int64
}

// testWindowSeconds is the deposit window used by RunTest. Smaller than the
// production ceremony window (600s) since the test immediately picks up and
// revokes; using the production window would needlessly hold escrow state at
// namek for the full TTL after a failed test.
const testWindowSeconds = 60

// ErrTestPreconditions is returned when RunTest is called from a state where
// the test cannot meaningfully run (manager locked, posture off, identity not
// ready). HTTP handler maps to 412 Precondition Failed.
var ErrTestPreconditions = errors.New("autounlock: test preconditions not met")

// RunTest exercises the full namek round-trip without touching the on-disk
// blob or the manager's SDEK. Generates a fresh random F, deposits, picks up,
// verifies byte-equality, revokes. Validates that namek connectivity works
// for THIS device's AK; does NOT validate the local crypto path or the
// shutdown-ceremony / boot-pickup orchestration.
func (o *Orchestrator) RunTest(ctx context.Context) (TestResult, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	state, err := LoadState()
	if err != nil && err != ErrInvalidStateFile {
		return TestResult{}, err
	}
	if state.Posture == PostureOff {
		return TestResult{}, ErrTestPreconditions
	}
	if o.deps.Manager.IsLocked() {
		return TestResult{}, ErrTestPreconditions
	}
	if !o.deps.IsIdentityReady() {
		return TestResult{}, ErrTestPreconditions
	}
	client := o.deps.NamekClient()
	if client == nil {
		return TestResult{}, ErrTestPreconditions
	}

	start := o.deps.Now()

	F := make([]byte, fSize)
	defer cryptoutil.SecureZero(F)
	if _, err := io.ReadFull(rand.Reader, F); err != nil {
		o.emitTestRun(false, ReasonDepositFailed, start)
		return TestResult{Success: false, ErrorKind: ReasonDepositFailed}, nil
	}

	if _, err := client.DepositUnlockEscrow(ctx, F, testWindowSeconds); err != nil {
		kind := classifyTestErr(err)
		o.emitTestRun(false, kind, start)
		return TestResult{Success: false, ErrorKind: kind}, nil
	}

	resp, err := client.PickupUnlockEscrow(ctx)
	if err != nil {
		// Best-effort revoke since deposit succeeded but pickup didn't —
		// don't leave orphan state at namek.
		_ = client.RevokeUnlockEscrow(ctx)
		kind := classifyTestErr(err)
		o.emitTestRun(false, kind, start)
		return TestResult{Success: false, ErrorKind: kind}, nil
	}

	got, err := base64.RawURLEncoding.DecodeString(resp.Secret)
	if err != nil {
		_ = client.RevokeUnlockEscrow(ctx)
		o.emitTestRun(false, ReasonBlobCorrupt, start)
		return TestResult{Success: false, ErrorKind: ReasonBlobCorrupt}, nil
	}
	defer cryptoutil.SecureZero(got)

	if !bytes.Equal(F, got) {
		_ = client.RevokeUnlockEscrow(ctx)
		o.emitTestRun(false, ReasonBlobCorrupt, start)
		return TestResult{Success: false, ErrorKind: ReasonBlobCorrupt}, nil
	}

	// Revoke synchronously — the test contract is "no escrow state at namek
	// after RunTest returns." Revoke errors map to test failure since they
	// indicate transient namek trouble worth surfacing to the user.
	if err := client.RevokeUnlockEscrow(ctx); err != nil {
		kind := classifyTestErr(err)
		o.emitTestRun(false, kind, start)
		return TestResult{Success: false, ErrorKind: kind}, nil
	}

	o.emitTestRun(true, "", start)
	latency := o.deps.Now().Sub(start).Milliseconds()
	return TestResult{Success: true, LatencyMs: latency}, nil
}

func (o *Orchestrator) emitTestRun(success bool, errorKind string, start time.Time) {
	latency := o.deps.Now().Sub(start).Milliseconds()
	details := map[string]any{
		"success":    success,
		"latency_ms": latency,
	}
	if !success {
		details["error_kind"] = errorKind
	}
	o.emitAudit(AuditTestRun, details)
}

// classifyTestErr maps a namek client error into the failure-reason token
// vocabulary used by audit emit and the UI failure-string map. Mirrors the
// pickup goroutine's classification logic.
func classifyTestErr(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return ReasonServiceUnreachable
	}
	// auth_failed (401/403) maps via APIError; anything else is "service
	// unreachable" from the user's perspective.
	var apiErr *namekclient.APIError
	if errors.As(err, &apiErr) && (apiErr.StatusCode == 401 || apiErr.StatusCode == 403) {
		return ReasonAuthFailed
	}
	return ReasonServiceUnreachable
}
