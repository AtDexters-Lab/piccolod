package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"piccolod/internal/health"
)

// recoveryRunner drives checkAndRecover: the OS update path reports a failure
// (transactional-update.service exit `lastRunExit`), nothing is in progress, and
// the active/default snapshots agree (so status is not Pending). It counts the
// systemd-run launches (real fires).
type recoveryRunner struct {
	fires       int32
	lastRunExit int
	distinct    bool // when true, each "show" reports a new failure timestamp
	seq         int32
	launchFails bool // when true, systemd-run fails to launch the transient unit
}

func (r *recoveryRunner) Run(ctx context.Context, name string, args ...string) (string, string, int, error) {
	switch name {
	case "systemd-run":
		atomic.AddInt32(&r.fires, 1)
		if r.launchFails {
			return "", "boom", 1, fmt.Errorf("systemd-run failed")
		}
		return "", "", 0, nil
	case "systemctl":
		if len(args) > 0 {
			switch args[0] {
			case "list-units":
				return "", "", 0, nil // nothing running
			case "is-active":
				return "", "", 3, nil // not active
			case "show":
				sec := 0
				if r.distinct {
					sec = int(atomic.AddInt32(&r.seq, 1)) // distinct RanAt per call
				}
				// Real failed Type=oneshot run: ActiveEnterTimestamp is EMPTY (never
				// became active); the run time is in ExecMainExitTimestamp.
				return fmt.Sprintf("ActiveEnterTimestamp=\nResult=failed\nExecMainStatus=%d\nExecMainExitTimestamp=Mon 2025-11-24 09:59:%02d UTC", r.lastRunExit, sec), "", 0, nil
			}
		}
		return "", "", 0, nil
	case "findmnt":
		return "/dev/sda3[/@/.snapshots/5/snapshot]\n", "", 0, nil
	case "btrfs":
		if len(args) >= 2 && args[0] == "subvolume" && args[1] == "get-default" {
			return "ID 5 (/.snapshots/5/snapshot)", "", 0, nil
		}
		return "", "", 0, nil // subvolume list -> empty -> snapperNumberFromID falls back to id
	case "snapper":
		return `{"configs":[{"config":"root","snapshots":[{"number":2,"date":"2025-11-20 09:00:00","description":"prev"},{"number":5,"date":"2025-11-21 09:00:00","description":"active"}]}]}`, "", 0, nil
	case "zypper":
		return "<update/>", "", 0, nil
	case "rpm":
		return "1.2.3-1\n", "", 0, nil
	case "journalctl":
		return "log", "", 0, nil
	}
	return "", "", 0, nil
}

// showRunner returns a fixed systemctl-show payload (and nothing else), to drive
// lastRunInfo against real captured output.
type showRunner struct{ out string }

func (r showRunner) Run(ctx context.Context, name string, args ...string) (string, string, int, error) {
	if name == "systemctl" && len(args) > 0 && args[0] == "show" {
		return r.out, "", 0, nil
	}
	return "", "", 0, nil
}

// Regression test for the real systemctl-show field order (captured on-device).
// Positional parsing made ExitCode always 0 / RanAt always nil here.
func TestLastRunInfoParsesRealSystemctlOrder(t *testing.T) {
	cases := []struct {
		name       string
		out        string
		wantExit   int
		wantRanAt  bool
		wantResult string
	}{
		// Real on-device captures. A failed oneshot has EMPTY ActiveEnterTimestamp
		// but a set ExecMainExitTimestamp — the case the old code couldn't detect.
		{"failed-run", "ActiveEnterTimestamp=\nResult=exit-code\nExecMainStatus=1\nExecMainExitTimestamp=Tue 2026-05-26 13:19:46 IST", 1, true, "exit-code"},
		{"never-run", "ActiveEnterTimestamp=\nResult=success\nExecMainStatus=0\nExecMainExitTimestamp=", 0, false, "success"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newRecoveryBackend(t, showRunner{tc.out}, 10<<30, nil, nil)
			info := m.lastRunInfo(context.Background())
			if info == nil {
				t.Fatal("lastRunInfo returned nil")
			}
			if info.ExitCode != tc.wantExit {
				t.Fatalf("ExitCode = %d, want %d", info.ExitCode, tc.wantExit)
			}
			if (info.RanAt != nil) != tc.wantRanAt {
				t.Fatalf("RanAt present = %v, want %v", info.RanAt != nil, tc.wantRanAt)
			}
			if info.Result != tc.wantResult {
				t.Fatalf("Result = %q, want %q", info.Result, tc.wantResult)
			}
		})
	}
}

// parseSystemdTime must resolve a local zone abbreviation (e.g. IST) to its real
// offset using the live system zone, not Go's process-cached time.Local. A
// fabricated 0-offset would skew RanAt and break checkAndRecover's 5b guard.
func TestParseSystemdTimeResolvesLocalZoneOffset(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Skip("tzdata for Asia/Kolkata unavailable")
	}
	old := systemLocation
	systemLocation = func() *time.Location { return loc }
	defer func() { systemLocation = old }()

	ts := parseSystemdTime("Thu 2026-05-21 12:28:00 IST")
	if ts == nil {
		t.Fatal("parseSystemdTime returned nil for a valid IST timestamp")
	}
	if _, off := ts.Zone(); off != 5*3600+30*60 {
		t.Fatalf("offset = %ds, want %ds (IST +0530) — abbreviation not resolved to real offset", off, 5*3600+30*60)
	}
}

func newRecoveryBackend(t *testing.T, r commandRunner, freeBytes uint64, clock func() time.Time, report func(health.Level, string)) *microOSBackend {
	t.Helper()
	dir := t.TempDir()
	opts := []Option{
		WithRunner(r),
		WithSupportOverride(true),
		WithStateDir(dir),
		WithRuntimeDir(filepath.Join(dir, "run")),
		WithFreeBytesFn(func(string) (uint64, error) { return freeBytes, nil }),
	}
	if clock != nil {
		opts = append(opts, WithClock(clock))
	}
	if report != nil {
		opts = append(opts, WithHealthReporter(report))
	}
	m, err := newMicroOSBackend(opts...)
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	return m
}

// The gate refuses a snapshot-creating update when free space is below the floor.
func TestDiskHeadroomGateBlocksApply(t *testing.T) {
	r := &recoveryRunner{lastRunExit: 1}
	m := newRecoveryBackend(t, r, 1<<30 /* 1 GiB < 2 GiB floor */, nil, nil)
	if err := m.Apply(context.Background()); !errors.Is(err, ErrInsufficientDisk) {
		t.Fatalf("Apply: want ErrInsufficientDisk, got %v", err)
	}
	if got := atomic.LoadInt32(&r.fires); got != 0 {
		t.Fatalf("expected no launch when gated, got %d", got)
	}
}

func TestDiskHeadroomGateAllowsWhenSpace(t *testing.T) {
	r := &recoveryRunner{lastRunExit: 1}
	m := newRecoveryBackend(t, r, 10<<30, nil, nil)
	if err := m.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: want nil, got %v", err)
	}
	if got := atomic.LoadInt32(&r.fires); got != 1 {
		t.Fatalf("expected 1 launch, got %d", got)
	}
}

// Rollback is the escape from a full disk and must never be gated.
func TestRollbackNotGatedByDisk(t *testing.T) {
	r := &recoveryRunner{}
	m := newRecoveryBackend(t, r, 0 /* no free space */, nil, nil)
	if err := m.Rollback(context.Background(), "2"); err != nil {
		t.Fatalf("Rollback must be exempt from the disk gate, got %v", err)
	}
}

// The core regression test: with a stream of *distinct* failed runs, recovery
// fires at most maxAutoRecoveryPerWindow times per window, then throttles. And it
// must never report LevelError — that feeds the readiness probe and could trigger
// a boot rollback of an otherwise-healthy system.
func TestAutoRecoveryBreakerBoundsFiring(t *testing.T) {
	r := &recoveryRunner{lastRunExit: 1, distinct: true}
	fixed := time.Unix(1700000000, 0)
	var levels []health.Level
	m := newRecoveryBackend(t, r, 10<<30, func() time.Time { return fixed },
		func(l health.Level, _ string) { levels = append(levels, l) })

	for i := 0; i < 6; i++ {
		m.checkAndRecover(context.Background())
	}

	if got := atomic.LoadInt32(&r.fires); got != maxAutoRecoveryPerWindow {
		t.Fatalf("fires: want %d (capped), got %d", maxAutoRecoveryPerWindow, got)
	}
	sawWarn := false
	for _, l := range levels {
		if l == health.LevelError {
			t.Fatalf("auto-recovery must not report LevelError (feeds boot-rollback): %v", levels)
		}
		if l == health.LevelWarn {
			sawWarn = true
		}
	}
	if !sawWarn {
		t.Fatalf("expected a LevelWarn escalation, got %v", levels)
	}
}

// After the operator manually responds (rollback/apply) to a failed run,
// auto-recovery must not fire on top of their fix.
func TestAutoRecoverySuppressedAfterUserAction(t *testing.T) {
	r := &recoveryRunner{lastRunExit: 1}
	clk := func() time.Time { return time.Date(2025, 11, 24, 10, 0, 0, 0, time.UTC) } // after the failure
	m := newRecoveryBackend(t, r, 10<<30, clk, nil)

	m.persistState("rollback", "2", "piccolo-tu-rollback-1", 0, "ok") // operator rolled back
	m.checkAndRecover(context.Background())

	if got := atomic.LoadInt32(&r.fires); got != 0 {
		t.Fatalf("auto-recovery must not fire after an operator rollback, got %d fires", got)
	}
}

// When this version is delivered by an older daemon's auto-fallback, state.json
// records that response but the ledger doesn't exist yet; we must still treat the
// failure as handled and not fire a redundant recovery on first boot.
func TestAutoRecoveryHonorsLegacyAutoFallbackState(t *testing.T) {
	r := &recoveryRunner{lastRunExit: 1}
	clk := func() time.Time { return time.Date(2025, 11, 24, 10, 0, 0, 0, time.UTC) } // after the failure
	m := newRecoveryBackend(t, r, 10<<30, clk, nil)

	m.persistState("auto-fallback", "", "piccolo-tu-auto-fallback-1", 0, "ok") // legacy state, no ledger
	m.checkAndRecover(context.Background())

	if got := atomic.LoadInt32(&r.fires); got != 0 {
		t.Fatalf("auto-recovery must honor legacy auto-fallback state, got %d fires", got)
	}
}

// A launch failure (no snapshot created) must not consume a breaker slot or mark
// the failure handled — otherwise transient systemd/D-Bus errors would falsely
// exhaust the budget and suppress real recovery.
func TestAutoRecoveryLaunchFailureReleasesSlot(t *testing.T) {
	r := &recoveryRunner{lastRunExit: 1, launchFails: true}
	fixed := time.Unix(1700000000, 0)
	m := newRecoveryBackend(t, r, 10<<30, func() time.Time { return fixed }, nil)

	m.checkAndRecover(context.Background())

	if m.autoRecoveryCount != 0 {
		t.Fatalf("a failed launch must not consume a breaker slot, got count=%d", m.autoRecoveryCount)
	}
	if !m.lastHandledFailure.IsZero() {
		t.Fatalf("a failed launch must not mark the failure handled")
	}
}

// A single failed run must trigger exactly one recovery, not one per tick — our
// fallback runs as a separate unit and doesn't clear the sampled last_run.
func TestAutoRecoveryDedupsSameFailure(t *testing.T) {
	r := &recoveryRunner{lastRunExit: 1} // distinct=false -> same RanAt every tick
	fixed := time.Unix(1700000000, 0)
	m := newRecoveryBackend(t, r, 10<<30, func() time.Time { return fixed }, nil)

	for i := 0; i < 5; i++ {
		m.checkAndRecover(context.Background())
	}
	if got := atomic.LoadInt32(&r.fires); got != 1 {
		t.Fatalf("same failed run must trigger exactly one recovery, got %d fires", got)
	}
}

func TestAutoRecoveryWindowReArms(t *testing.T) {
	cur := time.Unix(1700000000, 0)
	m := newRecoveryBackend(t, &recoveryRunner{}, 10<<30, func() time.Time { return cur }, nil)

	for i := 0; i < maxAutoRecoveryPerWindow; i++ {
		if !m.tryReserveAutoRecovery() {
			t.Fatalf("reservation %d should succeed before the cap", i)
		}
	}
	if m.tryReserveAutoRecovery() {
		t.Fatalf("expected breaker open at cap")
	}
	cur = cur.Add(autoRecoveryWindow + time.Minute)
	if !m.tryReserveAutoRecovery() { // rolls the window, count back to 1
		t.Fatalf("expected breaker to re-arm after window expiry")
	}
	if m.autoRecoveryCount != 1 {
		t.Fatalf("expected count reset to 1 after window roll, got %d", m.autoRecoveryCount)
	}
}

// Concurrent ticks (the immediate-check goroutine + the 15-min ticker) must not
// both slip past the cap. Run under -race.
func TestAutoRecoveryBreakerBoundsUnderConcurrency(t *testing.T) {
	m := newRecoveryBackend(t, &recoveryRunner{}, 10<<30,
		func() time.Time { return time.Unix(1700000000, 0) }, nil)

	var wg sync.WaitGroup
	var grants int32
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if m.tryReserveAutoRecovery() {
				atomic.AddInt32(&grants, 1)
			}
		}()
	}
	wg.Wait()

	if grants != int32(maxAutoRecoveryPerWindow) {
		t.Fatalf("concurrent reservations must not exceed the cap: got %d want %d", grants, maxAutoRecoveryPerWindow)
	}
	if m.autoRecoveryCount != maxAutoRecoveryPerWindow {
		t.Fatalf("count must equal cap, got %d", m.autoRecoveryCount)
	}
}

func TestAutoRecoveryResetClearsAndReportsOnce(t *testing.T) {
	var levels []health.Level
	m := newRecoveryBackend(t, &recoveryRunner{}, 10<<30, nil,
		func(l health.Level, _ string) { levels = append(levels, l) })

	m.tryReserveAutoRecovery()
	m.tryReserveAutoRecovery()
	m.resetAutoRecovery()
	if m.autoRecoveryCount != 0 || !m.autoRecoveryWindowStart.IsZero() {
		t.Fatalf("reset should clear the breaker, got count=%d", m.autoRecoveryCount)
	}
	before := len(levels)
	if before == 0 || levels[before-1] != health.LevelOK {
		t.Fatalf("expected a LevelOK report on reset, got %v", levels)
	}
	m.resetAutoRecovery() // already clear -> no additional report
	if len(levels) != before {
		t.Fatalf("redundant reset should not re-report health")
	}
}

func TestRecoveryLedgerSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	mk := func() *microOSBackend {
		m, err := newMicroOSBackend(
			WithRunner(&recoveryRunner{}),
			WithSupportOverride(true),
			WithStateDir(dir),
			WithRuntimeDir(filepath.Join(dir, "run")),
			WithFreeBytesFn(func(string) (uint64, error) { return 10 << 30, nil }),
			WithClock(func() time.Time { return time.Unix(1700000000, 0) }),
		)
		if err != nil {
			t.Fatalf("backend: %v", err)
		}
		return m
	}
	m1 := mk()
	m1.tryReserveAutoRecovery()
	m1.tryReserveAutoRecovery()

	m2 := mk() // simulate restart against the same state dir
	if m2.autoRecoveryCount != 2 {
		t.Fatalf("expected count 2 restored from ledger, got %d", m2.autoRecoveryCount)
	}
}

// Under a read-only / full fs the ledger write fails, but the in-memory count is
// authoritative and must still bound firing (the B1 failure mode).
func TestRecoveryCountBoundsWhenPersistFails(t *testing.T) {
	m := newRecoveryBackend(t, &recoveryRunner{}, 10<<30,
		func() time.Time { return time.Unix(1700000000, 0) }, nil)

	bad := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(bad, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.recoveryPath = filepath.Join(bad, "recovery.json") // MkdirAll under a file fails

	for i := 0; i < maxAutoRecoveryPerWindow; i++ {
		m.tryReserveAutoRecovery()
	}
	if m.tryReserveAutoRecovery() {
		t.Fatalf("breaker must still bound when ledger persistence fails")
	}
	if m.autoRecoveryCount != maxAutoRecoveryPerWindow {
		t.Fatalf("in-memory count should remain authoritative, got %d", m.autoRecoveryCount)
	}
}

func TestEscalateHealthSetOnce(t *testing.T) {
	var n int
	m := newRecoveryBackend(t, &recoveryRunner{}, 10<<30, nil,
		func(health.Level, string) { n++ })

	m.escalateHealth(health.LevelWarn, "x")
	m.escalateHealth(health.LevelWarn, "x") // duplicate -> suppressed
	if n != 1 {
		t.Fatalf("set-once: want 1 report, got %d", n)
	}
	m.escalateHealth(health.LevelError, "y") // changed -> reported
	if n != 2 {
		t.Fatalf("want 2 reports after a change, got %d", n)
	}
}
