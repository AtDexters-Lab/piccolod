package autounlock

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"time"

	"piccolod/internal/cryptoutil"

	"github.com/AtDexters-Lab/namek-server/pkg/namekclient"
)

// TestResult is returned by RunTest. ErrorKind is one of the failure-reason
// tokens (`service_unreachable`, `auth_failed`, etc.) when Success is false;
// empty otherwise. LatencyMs measures the full deposit + pickup
// round-trip.
type TestResult struct {
	Success   bool
	ErrorKind string
	LatencyMs int64
}

// testWindowSeconds is the deposit window used by RunTest. Smaller than the
// production ceremony window (600s). The remote singleton factor is not
// revoked because Namek v1 revoke is unkeyed; it expires after this short TTL.
const testWindowSeconds = 60

// ErrTestPreconditions is returned when RunTest is called from a state where
// the test cannot meaningfully run (manager locked, auto-unlock disabled,
// identity not ready). HTTP handler maps to 412 Precondition Failed.
var ErrTestPreconditions = errors.New("autounlock: test preconditions not met")

// ErrTestRateLimit is returned when RunTest is called within testRateLimit of
// the last invocation. HTTP handler maps to 429 Too Many Requests with
// Retry-After. Closes the "admin script clicks Test in a loop" surface that
// would otherwise bombard the recovery provider and starve other operations.
var ErrTestRateLimit = errors.New("autounlock: test rate limited (≥5s between calls)")

// testRateLimit is the minimum gap between successive RunTest invocations.
// Plan §"Test action" step 3 mandates ≥5s. Tunable as a Deps field would be
// over-engineering for a single-user appliance.
const testRateLimit = 5 * time.Second

// RunTest exercises the full provider round-trip without touching the on-disk
// blob or the manager's SDEK. Generates a fresh random F, deposits, picks up,
// and verifies byte-equality. Validates that provider connectivity works for
// THIS device; does NOT validate the local crypto path or the
// shutdown-ceremony / boot-pickup orchestration.
func (o *Orchestrator) RunTest(ctx context.Context) (TestResult, error) {
	if err := o.acquire(ctx); err != nil {
		return TestResult{}, err
	}
	defer o.release()

	// The provider is a singleton slot in v1. Never let Test overwrite a
	// factor that an outstanding raw blob still needs, regardless of whether
	// its optional metadata is understood.
	if _, err := ReadBlob(); err == nil {
		return TestResult{}, ErrHandoffBusy
	} else if !errors.Is(err, ErrBlobMissing) {
		return TestResult{}, err
	}

	now := o.deps.Now()
	if !o.lastTestAt.IsZero() && now.Sub(o.lastTestAt) < testRateLimit {
		return TestResult{}, ErrTestRateLimit
	}
	// Charge cooldown before precondition checks — closes loop-clicking DoS
	// on misconfigured devices.
	o.lastTestAt = now

	state, err := LoadState()
	if err != nil && !errors.Is(err, ErrInvalidStateFile) {
		return TestResult{}, err
	}
	if err == nil && len(state.Handoff) != 0 {
		// Metadata without a raw blob is orphaned and cannot authorize pickup.
		state.Handoff = nil
		if err := SaveState(state); err != nil {
			return TestResult{}, err
		}
	}
	if !state.Enabled {
		return TestResult{}, ErrTestPreconditions
	}
	if !o.deps.Manager.SDEKLoaded() {
		return TestResult{}, ErrTestPreconditions
	}
	if !o.providerReady() {
		return TestResult{}, ErrTestPreconditions
	}
	binding, ok := o.configuredProvider()
	if !ok {
		return TestResult{}, ErrTestPreconditions
	}

	start := o.deps.Now()

	F := make([]byte, fSize)
	defer cryptoutil.SecureZero(F)
	if _, err := io.ReadFull(rand.Reader, F); err != nil {
		o.emitTestRun(false, ReasonDepositFailed, start)
		return TestResult{Success: false, ErrorKind: ReasonDepositFailed}, nil
	}

	if _, err := binding.provider.Deposit(ctx, F, testWindowSeconds*time.Second); err != nil {
		kind := classifyTestErr(err)
		o.emitTestRun(false, kind, start)
		return TestResult{Success: false, ErrorKind: kind}, nil
	}

	got, err := binding.provider.Pickup(ctx)
	if err != nil {
		kind := classifyTestErr(err)
		o.emitTestRun(false, kind, start)
		return TestResult{Success: false, ErrorKind: kind}, nil
	}

	defer cryptoutil.SecureZero(got)

	if !bytes.Equal(F, got) {
		o.emitTestRun(false, ReasonBlobCorrupt, start)
		return TestResult{Success: false, ErrorKind: ReasonBlobCorrupt}, nil
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
	if errors.Is(err, ErrRecoveryFactorInvalid) {
		return ReasonBlobCorrupt
	}
	// auth_failed (401/403) maps via APIError; anything else is "service
	// unreachable" from the user's perspective.
	var apiErr *namekclient.APIError
	if errors.As(err, &apiErr) && (apiErr.StatusCode == 401 || apiErr.StatusCode == 403) {
		return ReasonAuthFailed
	}
	return ReasonServiceUnreachable
}
