package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestStatusWithStagedSnapshot(t *testing.T) {
	tmp := t.TempDir()
	clock := func() time.Time { return time.Date(2025, 11, 24, 10, 0, 0, 0, time.UTC) }
	mockReadFile := func(path string) ([]byte, error) {
		if path == "/etc/os-release" {
			return []byte(`VERSION_ID="20251124"`), nil
		}
		if strings.Contains(path, "/.snapshots/7/snapshot/etc/os-release") {
			return []byte(`VERSION_ID="20251125"`), nil
		}
		return nil, os.ErrNotExist
	}
	m, err := NewManager(
		WithRunner(fakeRunner{}),
		WithStateDir(tmp),
		WithRuntimeDir(filepath.Join(tmp, "run")),
		WithClock(clock),
		WithSupportOverride(true),
		WithReadFile(mockReadFile),
		WithCurrentVersion("1.2.3-1"),
	)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	// Seed a pending apply intent to check derived_outcome
	state := persistedState{LastAction: "apply", TargetSnapshot: "7", RequestedAt: clock()}
	b, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(tmp, "update", "state.json"), b, 0o600); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	st, err := m.Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !st.Pending || !st.RequiresReboot {
		t.Fatalf("expected pending/requires_reboot true, got %+v", st)
	}
	// Expectation: piccolod RPM version (1.2.3-1 / 1.2.4-1) takes precedence over OS version (20251124)
	if st.CurrentVersion != "1.2.3-1" {
		t.Fatalf("expected current version 1.2.3-1 (RPM), got %s", st.CurrentVersion)
	}
	if st.AvailableVersion != "1.2.4-1" {
		t.Fatalf("expected available version 1.2.4-1 (RPM), got %s", st.AvailableVersion)
	}
	meta := st.Meta
	if meta == nil {
		t.Fatalf("expected meta to be populated")
	}
	if meta["derived_outcome"] != "pending-reboot" {
		t.Fatalf("expected derived_outcome pending-reboot, got %v", meta["derived_outcome"])
	}
	if meta["piccolod_active"] == meta["piccolod_staged"] {
		t.Fatalf("expected staged piccolod version to differ")
	}
	if cnt, ok := meta["rpm_updates_available"].(int); !ok || cnt != 2 {
		t.Fatalf("expected rpm_updates_available=2, got %v", meta["rpm_updates_available"])
	}
}

func TestStatusFallbackToSystemUpdateID(t *testing.T) {
	tmp := t.TempDir()
	noRpmRunner := &noRpmRunner{}
	mockReadFile := func(path string) ([]byte, error) {
		return nil, os.ErrNotExist // Simulate missing os-release
	}

	m, err := NewManager(
		WithRunner(noRpmRunner),
		WithStateDir(tmp),
		WithRuntimeDir(filepath.Join(tmp, "run")),
		WithSupportOverride(true),
		WithReadFile(mockReadFile),
		WithCurrentVersion("v0.1.0"),
	)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	st, err := m.Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}

	expected := "v0.1.0 (System Update 7)"
	if st.AvailableVersion != expected {
		t.Fatalf("expected available version %q, got %q", expected, st.AvailableVersion)
	}
}

type noRpmRunner struct{ fakeRunner }

func (r *noRpmRunner) Run(ctx context.Context, name string, args ...string) (string, string, int, error) {
	if name == "rpm" {
		return "", "package piccolod is not installed", 1, nil
	}
	return r.fakeRunner.Run(ctx, name, args...)
}

func TestTimeoutEnvOverride(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PICCOLO_UPDATE_TIMEOUT_S", "120")
	m, err := newMicroOSBackend(
		WithRunner(fakeRunner{}),
		WithStateDir(tmp),
		WithRuntimeDir(filepath.Join(tmp, "run")),
		WithSupportOverride(true),
	)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	if m.timeout != 120*time.Second {
		t.Fatalf("expected timeout 120s, got %v", m.timeout)
	}
}

func TestPickRollbackSkipsStaged(t *testing.T) {
	m, err := newMicroOSBackend(
		WithRunner(fakeRunner{}),
		WithSupportOverride(true),
		WithStateDir(t.TempDir()),
		WithRuntimeDir(filepath.Join(t.TempDir(), "run")),
	)
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	if id := m.pickRollbackTarget(context.Background()); id != "6" {
		t.Fatalf("expected rollback target 6 (skip staged 7), got %s", id)
	}
}

func TestRunTransactionalUpdateStopsOnTimeout(t *testing.T) {
	tmp := t.TempDir()
	r := &timeoutRunner{}
	m, err := newMicroOSBackend(
		WithRunner(r),
		WithClock(func() time.Time { return time.Unix(1700000000, 0) }),
		WithTimeout(5*time.Millisecond),
		WithStateDir(tmp),
		WithRuntimeDir(filepath.Join(tmp, "run")),
		WithSupportOverride(true),
		WithFreeBytesFn(func(string) (uint64, error) { return 10 << 30, nil }),
	)
	if err != nil {
		t.Fatalf("backend: %v", err)
	}

	err = m.runTransactionalUpdate(context.Background(), []string{"transactional-update", "dup"}, "apply", "", true)
	if err != ErrTimeout {
		t.Fatalf("expected ErrTimeout, got %v", err)
	}
	foundStop := false
	for _, c := range r.calls {
		if c.name == "systemctl" && len(c.args) >= 2 && c.args[0] == "stop" && strings.HasPrefix(c.args[1], "piccolo-tu-apply-1700000000") {
			foundStop = true
			break
		}
	}
	if !foundStop {
		t.Fatalf("expected systemctl stop of transient unit on timeout, got calls %#v", r.calls)
	}
}

type call struct {
	name string
	args []string
}

type timeoutRunner struct {
	calls []call
}

func (r *timeoutRunner) Run(ctx context.Context, name string, args ...string) (string, string, int, error) {
	r.calls = append(r.calls, call{name: name, args: args})
	switch name {
	case "systemctl":
		if len(args) > 0 && args[0] == "is-active" {
			return "", "", 3, nil
		}
		if len(args) > 0 && args[0] == "stop" {
			return "", "", 0, nil
		}
	case "systemd-run":
		// Sleep past the timeout to trigger deadline exceeded.
		time.Sleep(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			return "", "", -1, ctx.Err()
		default:
			return "", "", 0, nil
		}
	}
	return "", "", 0, nil
}

type fakeRunner struct{}

func (fakeRunner) Run(ctx context.Context, name string, args ...string) (string, string, int, error) {
	switch name {
	case "findmnt":
		return "/dev/sda3[/@/.snapshots/5/snapshot]\n", "", 0, nil
	case "btrfs":
		return "ID 7 (/.snapshots/7/snapshot)", "", 0, nil
	case "snapper":
		return `{"configs":[{"config":"root","snapshots":[{"number":5,"date":"2025-11-20 09:00:00","description":"active"},{"number":6,"date":"2025-11-21 09:00:00","description":"prev"},{"number":7,"date":"2025-11-23 10:00:00","description":"staged"}]}]}`, "", 0, nil
	case "systemctl":
		if len(args) > 0 && args[0] == "list-units" {
			return "", "", 0, nil
		}
		if len(args) > 0 && args[0] == "is-active" {
			return "", "", 3, nil
		}
		if len(args) > 0 && args[0] == "show" {
			return "ActiveEnterTimestamp=Mon 2025-11-24 09:59:00 UTC\nResult=success\nExecMainStatus=0\nExecMainExitTimestamp=Mon 2025-11-24 09:59:01 UTC", "", 0, nil
		}
		return "", "", 0, nil
	case "journalctl":
		return "txn ok\nprepared snapshot 7\n", "", 0, nil
	case "zypper":
		return "<update/><update/>", "", 0, nil
	case "rpm":
		// Distinguish staged vs active via --root flag
		for i, a := range args {
			if a == "--root" && i+1 < len(args) && args[i+1] != "" {
				return "1.2.4-1\n", "", 0, nil
			}
		}
		return "1.2.3-1\n", "", 0, nil
	default:
		return "", "", 0, nil
	}
}

// Runner that always reports a running piccolo-tu-* unit to trigger ErrInProgress.
type inProgressRunner struct{}

func (inProgressRunner) Run(ctx context.Context, name string, args ...string) (string, string, int, error) {
	switch name {
	case "systemctl":
		// List-units path
		if len(args) >= 5 && args[0] == "list-units" {
			return "piccolo-tu-apply.service loaded active running\n", "", 0, nil
		}
		// is-active transactional-update fallback
		if len(args) >= 2 && args[0] == "is-active" {
			return "", "", 0, nil
		}
	case "snapper":
		// Keep a small snapshot set so validation passes when not blocked earlier.
		return `{"configs":[{"config":"root","snapshots":[{"number":1,"date":"2025-11-20 09:00:00","description":"active"}]}]}`, "", 0, nil
	}
	return "", "", 0, nil
}

// Runner that serves a minimal snapshot list for rollback validation.
type snapshotRunner struct{}

func (snapshotRunner) Run(ctx context.Context, name string, args ...string) (string, string, int, error) {
	switch name {
	case "snapper":
		return `{"configs":[{"config":"root","snapshots":[{"number":1,"date":"2025-11-20 09:00:00","description":"active"},{"number":2,"date":"2025-11-21 09:00:00","description":"prev"}]}]}`, "", 0, nil
	default:
		return "", "", 0, nil
	}
}

func TestApplyReturnsInProgressWhenTUAlreadyRunning(t *testing.T) {
	m, err := newMicroOSBackend(
		WithRunner(inProgressRunner{}),
		WithSupportOverride(true),
		WithStateDir(t.TempDir()),
		WithRuntimeDir(filepath.Join(t.TempDir(), "run")),
	)
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	if err := m.Apply(context.Background()); err != ErrInProgress {
		t.Fatalf("expected ErrInProgress, got %v", err)
	}
}

// validationRunner wraps fakeRunner but records all calls for assertion
// and allows configuring active/default snapshot IDs.
type validationRunner struct {
	activeID  string // snapper number returned by findmnt
	defaultID string // btrfs subvolume ID returned by get-default
	calls     []call
	// inProgress controls whether isInProgress returns true
	inProgress bool
	// revertFails makes snapper modify --default return an error
	revertFails bool
}

func (r *validationRunner) Run(ctx context.Context, name string, args ...string) (string, string, int, error) {
	r.calls = append(r.calls, call{name: name, args: append([]string{}, args...)})
	switch name {
	case "findmnt":
		return "/dev/sda3[/@/.snapshots/" + r.activeID + "/snapshot]\n", "", 0, nil
	case "btrfs":
		if len(args) >= 2 && args[0] == "subvolume" && args[1] == "get-default" {
			if r.defaultID == "" {
				return "", "", 0, nil
			}
			return "ID " + r.defaultID + " gen 59 top level 257 path @/.snapshots/" + r.defaultID + "/snapshot", "", 0, nil
		}
		if len(args) >= 2 && args[0] == "subvolume" && args[1] == "list" {
			// Return both active and default entries
			lines := "ID " + r.activeID + " gen 50 top level 257 path @/.snapshots/" + r.activeID + "/snapshot\n"
			if r.defaultID != "" && r.defaultID != r.activeID {
				lines += "ID " + r.defaultID + " gen 59 top level 257 path @/.snapshots/" + r.defaultID + "/snapshot\n"
			}
			return lines, "", 0, nil
		}
		return "", "", 0, nil
	case "systemctl":
		if len(args) > 0 && args[0] == "list-units" {
			if r.inProgress {
				return "piccolo-tu-apply.service loaded active running\n", "", 0, nil
			}
			return "", "", 0, nil
		}
		if len(args) > 0 && args[0] == "is-active" {
			return "", "", 3, nil
		}
		if len(args) > 0 && args[0] == "show" {
			return "ActiveEnterTimestamp=Mon 2025-11-24 09:59:00 UTC\nResult=success\nExecMainStatus=0\nExecMainExitTimestamp=Mon 2025-11-24 09:59:01 UTC", "", 0, nil
		}
		return "", "", 0, nil
	case "snapper":
		if len(args) >= 2 && args[0] == "modify" {
			if r.revertFails {
				return "", "error", 1, fmt.Errorf("snapper modify failed")
			}
			return "", "", 0, nil
		}
		if len(args) >= 1 && args[0] == "delete" {
			return "", "", 0, nil
		}
		// snapper --json list
		return `{"configs":[{"config":"root","snapshots":[{"number":5},{"number":7}]}]}`, "", 0, nil
	case "journalctl":
		return "", "", 0, nil
	case "zypper":
		return "", "", 0, nil
	case "rpm":
		return "", "not installed", 1, nil
	default:
		return "", "", 0, nil
	}
}

func (r *validationRunner) hasCall(name string, argSubstrings ...string) bool {
	for _, c := range r.calls {
		if c.name != name {
			continue
		}
		joined := strings.Join(c.args, " ")
		match := true
		for _, sub := range argSubstrings {
			if !strings.Contains(joined, sub) {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func (r *validationRunner) countCalls(name string, argSubstrings ...string) int {
	count := 0
	for _, c := range r.calls {
		if c.name != name {
			continue
		}
		joined := strings.Join(c.args, " ")
		match := true
		for _, sub := range argSubstrings {
			if !strings.Contains(joined, sub) {
				match = false
				break
			}
		}
		if match {
			count++
		}
	}
	return count
}

func TestSnapshotStateFastPath(t *testing.T) {
	t.Run("staged_without_snapper", func(t *testing.T) {
		r := &validationRunner{activeID: "5", defaultID: "7"}
		m, err := newMicroOSBackend(
			WithRunner(r),
			WithStateDir(t.TempDir()),
			WithRuntimeDir(filepath.Join(t.TempDir(), "run")),
			WithSupportOverride(true),
		)
		if err != nil {
			t.Fatalf("backend: %v", err)
		}

		st, err := m.SnapshotState(context.Background())
		if err != nil {
			t.Fatalf("SnapshotState: %v", err)
		}
		if st.Readiness != SnapshotReadinessStaged || !st.RequiresReboot {
			t.Fatalf("SnapshotState = %+v, want staged/reboot", st)
		}
		if st.ActiveSnapshot != "5" || st.DefaultSnapshot != "7" {
			t.Fatalf("unexpected snapshots: %+v", st)
		}
		if r.hasCall("snapper") || r.hasCall("zypper") || r.hasCall("rpm") {
			t.Fatalf("fast snapshot path must not call enrichment tools: %#v", r.calls)
		}
	})

	t.Run("absent", func(t *testing.T) {
		r := &validationRunner{activeID: "5", defaultID: "5"}
		m, err := newMicroOSBackend(
			WithRunner(r),
			WithStateDir(t.TempDir()),
			WithRuntimeDir(filepath.Join(t.TempDir(), "run")),
			WithSupportOverride(true),
		)
		if err != nil {
			t.Fatalf("backend: %v", err)
		}

		st, err := m.SnapshotState(context.Background())
		if err != nil {
			t.Fatalf("SnapshotState: %v", err)
		}
		if st.Readiness != SnapshotReadinessAbsent || st.RequiresReboot {
			t.Fatalf("SnapshotState = %+v, want absent/no reboot", st)
		}
	})

	t.Run("in_progress_preempts_mismatch", func(t *testing.T) {
		r := &validationRunner{activeID: "5", defaultID: "7", inProgress: true}
		m, err := newMicroOSBackend(
			WithRunner(r),
			WithStateDir(t.TempDir()),
			WithRuntimeDir(filepath.Join(t.TempDir(), "run")),
			WithSupportOverride(true),
		)
		if err != nil {
			t.Fatalf("backend: %v", err)
		}

		st, err := m.SnapshotState(context.Background())
		if err != nil {
			t.Fatalf("SnapshotState: %v", err)
		}
		if st.Readiness != SnapshotReadinessInProgress || st.RequiresReboot {
			t.Fatalf("SnapshotState = %+v, want in_progress/no reboot", st)
		}
		if r.hasCall("findmnt") {
			t.Fatalf("in-progress state should preempt snapshot comparison: %#v", r.calls)
		}
	})
}

type rawDefaultNoMappingRunner struct{ validationRunner }

func (r *rawDefaultNoMappingRunner) Run(ctx context.Context, name string, args ...string) (string, string, int, error) {
	r.calls = append(r.calls, call{name: name, args: append([]string{}, args...)})
	switch name {
	case "findmnt":
		return "/dev/sda3[/@/.snapshots/5/snapshot]\n", "", 0, nil
	case "btrfs":
		if len(args) >= 2 && args[0] == "subvolume" && args[1] == "get-default" {
			return "ID 2716 gen 158761 top level 257", "", 0, nil
		}
		if len(args) >= 2 && args[0] == "subvolume" && args[1] == "list" {
			return "ID 999 gen 1 top level 257 path @/.snapshots/5/snapshot\n", "", 0, nil
		}
	case "systemctl":
		if len(args) > 0 && args[0] == "list-units" {
			return "", "", 0, nil
		}
		if len(args) > 0 && args[0] == "is-active" {
			return "", "", 3, nil
		}
	}
	return "", "", 0, nil
}

func TestSnapshotStateUnknownWhenDefaultCannotNormalize(t *testing.T) {
	r := &rawDefaultNoMappingRunner{}
	m, err := newMicroOSBackend(
		WithRunner(r),
		WithStateDir(t.TempDir()),
		WithRuntimeDir(filepath.Join(t.TempDir(), "run")),
		WithSupportOverride(true),
	)
	if err != nil {
		t.Fatalf("backend: %v", err)
	}

	st, err := m.SnapshotState(context.Background())
	if err == nil {
		t.Fatal("SnapshotState should error when default ID cannot normalize")
	}
	if st.Readiness != SnapshotReadinessUnknown {
		t.Fatalf("readiness = %q, want unknown", st.Readiness)
	}
}

func TestSnapshotStateUsesRequestTimeout(t *testing.T) {
	m, err := newMicroOSBackend(
		WithRunner(hangingStatusRunner{}),
		WithStateDir(t.TempDir()),
		WithRuntimeDir(filepath.Join(t.TempDir(), "run")),
		WithSupportOverride(true),
		WithStatusRequestTimeout(10*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("backend: %v", err)
	}

	start := time.Now()
	st, err := m.SnapshotState(context.Background())
	if err == nil {
		t.Fatal("SnapshotState should return a timeout error")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("SnapshotState took %v; expected bounded timeout", elapsed)
	}
	if st.Readiness != SnapshotReadinessUnknown {
		t.Fatalf("readiness = %q, want unknown", st.Readiness)
	}
}

func TestStatusPreservesErrInProgress(t *testing.T) {
	m, err := newMicroOSBackend(
		WithRunner(inProgressRunner{}),
		WithStateDir(t.TempDir()),
		WithRuntimeDir(filepath.Join(t.TempDir(), "run")),
		WithSupportOverride(true),
	)
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	if _, err := m.Status(context.Background()); !errors.Is(err, ErrInProgress) {
		t.Fatalf("Status: want ErrInProgress, got %v", err)
	}
}

type statusUnknownAfterCacheRunner struct {
	validationRunner
	unknown bool
}

func (r *statusUnknownAfterCacheRunner) Run(ctx context.Context, name string, args ...string) (string, string, int, error) {
	if !r.unknown {
		return r.validationRunner.Run(ctx, name, args...)
	}

	r.calls = append(r.calls, call{name: name, args: append([]string{}, args...)})
	switch name {
	case "findmnt":
		return "/dev/sda3[/@/.snapshots/5/snapshot]\n", "", 0, nil
	case "btrfs":
		if len(args) >= 2 && args[0] == "subvolume" && args[1] == "get-default" {
			return "ID 2716 gen 158761 top level 257", "", 0, nil
		}
		if len(args) >= 2 && args[0] == "subvolume" && args[1] == "list" {
			return "ID 999 gen 1 top level 257 path @/.snapshots/5/snapshot\n", "", 0, nil
		}
	case "systemctl":
		if len(args) > 0 && args[0] == "list-units" {
			return "", "", 0, nil
		}
		if len(args) > 0 && args[0] == "is-active" {
			return "", "", 3, nil
		}
	}
	return "", "", 0, nil
}

func TestStatusUnknownReadinessDoesNotReuseCachedRebootState(t *testing.T) {
	r := &statusUnknownAfterCacheRunner{
		validationRunner: validationRunner{activeID: "5", defaultID: "7"},
	}
	m, err := newMicroOSBackend(
		WithRunner(r),
		WithStateDir(t.TempDir()),
		WithRuntimeDir(filepath.Join(t.TempDir(), "run")),
		WithSupportOverride(true),
		WithCurrentVersion("v-test"),
		WithReadFile(func(string) ([]byte, error) { return nil, os.ErrNotExist }),
	)
	if err != nil {
		t.Fatalf("backend: %v", err)
	}

	st, err := m.Status(context.Background())
	if err != nil {
		t.Fatalf("prime Status: %v", err)
	}
	if !st.Pending || !st.RequiresReboot {
		t.Fatalf("prime status should report staged update, got %+v", st)
	}

	r.unknown = true
	st, err = m.Status(context.Background())
	if err != nil {
		t.Fatalf("unknown Status should degrade instead of erroring: %v", err)
	}
	if st.Pending || st.RequiresReboot {
		t.Fatalf("unknown readiness reused cached reboot flags: %+v", st)
	}
	if got := st.Meta["snapshot_readiness"]; got != string(SnapshotReadinessUnknown) {
		t.Fatalf("snapshot_readiness = %v, want unknown; meta=%v", got, st.Meta)
	}
	if degraded, _ := st.Meta["degraded"].(bool); !degraded {
		t.Fatalf("expected degraded meta, got %v", st.Meta)
	}
}

type slowEnrichmentRunner struct {
	validationRunner
}

func (r *slowEnrichmentRunner) Run(ctx context.Context, name string, args ...string) (string, string, int, error) {
	if name == "snapper" && len(args) >= 2 && args[0] == "--json" && args[1] == "list" {
		r.calls = append(r.calls, call{name: name, args: append([]string{}, args...)})
		<-ctx.Done()
		return "", "", -1, ctx.Err()
	}
	return r.validationRunner.Run(ctx, name, args...)
}

func TestStatusKeepsRequiresRebootWhenEnrichmentTimesOut(t *testing.T) {
	r := &slowEnrichmentRunner{validationRunner: validationRunner{activeID: "5", defaultID: "7"}}
	m, err := newMicroOSBackend(
		WithRunner(r),
		WithStateDir(t.TempDir()),
		WithRuntimeDir(filepath.Join(t.TempDir(), "run")),
		WithSupportOverride(true),
		WithCurrentVersion("v-test"),
		WithReadFile(func(string) ([]byte, error) { return nil, os.ErrNotExist }),
		WithStatusRequestTimeout(10*time.Millisecond),
		WithStatusRefreshTimeout(10*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("backend: %v", err)
	}

	st, err := m.Status(context.Background())
	if err != nil {
		t.Fatalf("Status should degrade instead of erroring: %v", err)
	}
	if !st.Pending || !st.RequiresReboot {
		t.Fatalf("expected pending/requires_reboot from fast snapshot state, got %+v", st)
	}
	if degraded, _ := st.Meta["degraded"].(bool); !degraded {
		t.Fatalf("expected degraded meta, got %v", st.Meta)
	}
	reason, _ := st.Meta["degraded_reason"].(string)
	if !strings.Contains(reason, "deadline") {
		t.Fatalf("degraded_reason = %q, want timeout detail", reason)
	}
}

func TestStatusBackoffPreservesEnrichmentFailureReason(t *testing.T) {
	r := &slowEnrichmentRunner{validationRunner: validationRunner{activeID: "5", defaultID: "7"}}
	m, err := newMicroOSBackend(
		WithRunner(r),
		WithStateDir(t.TempDir()),
		WithRuntimeDir(filepath.Join(t.TempDir(), "run")),
		WithSupportOverride(true),
		WithCurrentVersion("v-test"),
		WithReadFile(func(string) ([]byte, error) { return nil, os.ErrNotExist }),
		WithStatusRequestTimeout(10*time.Millisecond),
		WithStatusRefreshTimeout(10*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("backend: %v", err)
	}

	if _, err := m.Status(context.Background()); err != nil {
		t.Fatalf("first Status should degrade instead of erroring: %v", err)
	}
	st, err := m.Status(context.Background())
	if err != nil {
		t.Fatalf("backoff Status should degrade instead of erroring: %v", err)
	}
	if backoff, _ := st.Meta["enrichment_backoff"].(bool); !backoff {
		t.Fatalf("expected enrichment_backoff meta, got %v", st.Meta)
	}
	reason, _ := st.Meta["degraded_reason"].(string)
	if reason == "" || strings.Contains(reason, "status enrichment unavailable") {
		t.Fatalf("degraded_reason = %q, want original enrichment failure", reason)
	}
	if r.countCalls("snapper") != 1 {
		t.Fatalf("backoff Status should not retry snapper, calls=%#v", r.calls)
	}
}

type cancelingStatusRunner struct {
	cancel       context.CancelFunc
	snapperCalls atomic.Int32
}

func (r *cancelingStatusRunner) Run(ctx context.Context, name string, args ...string) (string, string, int, error) {
	switch name {
	case "findmnt":
		return "/dev/sda3[/@/.snapshots/5/snapshot]\n", "", 0, nil
	case "btrfs":
		if len(args) >= 2 && args[0] == "subvolume" && args[1] == "get-default" {
			return "ID 7 gen 59 top level 257 path @/.snapshots/7/snapshot", "", 0, nil
		}
		if len(args) >= 2 && args[0] == "subvolume" && args[1] == "list" {
			return "ID 5 gen 50 top level 257 path @/.snapshots/5/snapshot\nID 7 gen 59 top level 257 path @/.snapshots/7/snapshot\n", "", 0, nil
		}
	case "systemctl":
		if len(args) > 0 && args[0] == "list-units" {
			return "", "", 0, nil
		}
		if len(args) > 0 && args[0] == "is-active" {
			return "", "", 3, nil
		}
		if len(args) > 0 && args[0] == "show" {
			return "Result=success\nExecMainStatus=0\nExecMainExitTimestamp=Mon 2025-11-24 09:59:01 UTC", "", 0, nil
		}
	case "journalctl":
		return "", "", 0, nil
	case "snapper":
		if len(args) >= 2 && args[0] == "--json" && args[1] == "list" {
			if r.snapperCalls.Add(1) == 1 {
				r.cancel()
				<-ctx.Done()
				return "", "", -1, ctx.Err()
			}
			return `{"configs":[{"config":"root","snapshots":[{"number":5},{"number":7}]}]}`, "", 0, nil
		}
	case "zypper":
		return "<update/>", "", 0, nil
	case "rpm":
		for i, a := range args {
			if a == "--root" && i+1 < len(args) && args[i+1] != "" {
				return "1.2.4-1\n", "", 0, nil
			}
		}
		return "1.2.3-1\n", "", 0, nil
	}
	return "", "", 0, nil
}

func TestStatusClientCancellationDoesNotEnterEnrichmentBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &cancelingStatusRunner{cancel: cancel}
	m, err := newMicroOSBackend(
		WithRunner(r),
		WithStateDir(t.TempDir()),
		WithRuntimeDir(filepath.Join(t.TempDir(), "run")),
		WithSupportOverride(true),
		WithCurrentVersion("v-test"),
		WithReadFile(func(string) ([]byte, error) { return nil, os.ErrNotExist }),
		WithStatusRequestTimeout(time.Second),
		WithStatusRefreshTimeout(time.Second),
	)
	if err != nil {
		t.Fatalf("backend: %v", err)
	}

	st, err := m.Status(ctx)
	if err != nil {
		t.Fatalf("canceled Status should degrade locally instead of returning error: %v", err)
	}
	if !st.Pending || !st.RequiresReboot {
		t.Fatalf("expected pending/requires_reboot from fast snapshot state, got %+v", st)
	}

	deadline := time.Now().Add(time.Second)
	for {
		m.statusMu.RLock()
		cachedAt := m.statusCachedAt
		refreshing := m.statusRefreshActive
		backoffUntil := m.statusEnrichmentBackoffUntil
		lastErr := m.statusLastEnrichmentErr
		cached := m.statusCache
		m.statusMu.RUnlock()

		if !backoffUntil.IsZero() {
			t.Fatalf("client cancellation should not set enrichment backoff: %v", backoffUntil)
		}
		if lastErr != "" {
			t.Fatalf("client cancellation should not persist enrichment error: %q", lastErr)
		}
		if r.snapperCalls.Load() >= 2 && !refreshing && !cachedAt.IsZero() {
			if backoff, _ := cached.Meta["enrichment_backoff"].(bool); backoff {
				t.Fatalf("scheduled refresh published backoff metadata after client cancellation: %v", cached.Meta)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("scheduled refresh did not run after client cancellation; snapper_calls=%d refreshing=%v cached_at=%v", r.snapperCalls.Load(), refreshing, cachedAt)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type quickEnrichmentFailureRunner struct {
	calls []call
	fail  string
}

func (r *quickEnrichmentFailureRunner) Run(ctx context.Context, name string, args ...string) (string, string, int, error) {
	r.calls = append(r.calls, call{name: name, args: append([]string{}, args...)})
	switch name {
	case "findmnt":
		return "/dev/sda3[/@/.snapshots/5/snapshot]\n", "", 0, nil
	case "btrfs":
		if len(args) >= 2 && args[0] == "subvolume" && args[1] == "get-default" {
			return "ID 7 gen 59 top level 257 path @/.snapshots/7/snapshot", "", 0, nil
		}
		if len(args) >= 2 && args[0] == "subvolume" && args[1] == "list" {
			return "ID 5 gen 50 top level 257 path @/.snapshots/5/snapshot\nID 7 gen 59 top level 257 path @/.snapshots/7/snapshot\n", "", 0, nil
		}
	case "systemctl":
		if len(args) > 0 && args[0] == "list-units" {
			return "", "", 0, nil
		}
		if len(args) > 0 && args[0] == "is-active" {
			return "", "", 3, nil
		}
		if len(args) > 0 && args[0] == "show" {
			return "Result=success\nExecMainStatus=0\nExecMainExitTimestamp=Mon 2025-11-24 09:59:01 UTC", "", 0, nil
		}
	case "journalctl":
		return "", "", 0, nil
	case "snapper":
		if r.fail == "snapper" && len(args) >= 2 && args[0] == "--json" && args[1] == "list" {
			return "", "Config is locked.", 1, fmt.Errorf("snapper failed")
		}
		return `{"configs":[{"config":"root","snapshots":[{"number":5},{"number":7}]}]}`, "", 0, nil
	case "zypper":
		if r.fail == "zypper" {
			return "", "zypp lock is held", 1, fmt.Errorf("zypper failed")
		}
		return "<update/>", "", 0, nil
	case "rpm":
		if r.fail == "rpm" {
			return "", "rpmdb is locked", 1, fmt.Errorf("rpm failed")
		}
		for i, a := range args {
			if a == "--root" && i+1 < len(args) && args[i+1] != "" {
				return "1.2.4-1\n", "", 0, nil
			}
		}
		return "1.2.3-1\n", "", 0, nil
	}
	return "", "", 0, nil
}

func (r *quickEnrichmentFailureRunner) countCalls(name string, argSubstrings ...string) int {
	count := 0
	for _, c := range r.calls {
		if c.name != name {
			continue
		}
		joined := strings.Join(c.args, " ")
		match := true
		for _, sub := range argSubstrings {
			if !strings.Contains(joined, sub) {
				match = false
				break
			}
		}
		if match {
			count++
		}
	}
	return count
}

func TestStatusBackoffOnQuickEnrichmentFailures(t *testing.T) {
	cases := []struct {
		name          string
		fail          string
		command       string
		argSubstrings []string
		reason        string
	}{
		{name: "snapper", fail: "snapper", command: "snapper", argSubstrings: []string{"--json", "list"}, reason: "Config is locked"},
		{name: "zypper", fail: "zypper", command: "zypper", argSubstrings: []string{"--xmlout", "lu"}, reason: "zypp lock"},
		{name: "rpm", fail: "rpm", command: "rpm", argSubstrings: []string{"-q", "piccolod"}, reason: "rpmdb is locked"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &quickEnrichmentFailureRunner{fail: tc.fail}
			m, err := newMicroOSBackend(
				WithRunner(r),
				WithStateDir(t.TempDir()),
				WithRuntimeDir(filepath.Join(t.TempDir(), "run")),
				WithSupportOverride(true),
				WithCurrentVersion("v-test"),
				WithReadFile(func(string) ([]byte, error) { return nil, os.ErrNotExist }),
			)
			if err != nil {
				t.Fatalf("backend: %v", err)
			}

			st, err := m.Status(context.Background())
			if err != nil {
				t.Fatalf("Status should return degraded partial status, got error: %v", err)
			}
			if !st.Pending || !st.RequiresReboot {
				t.Fatalf("expected pending/requires_reboot from fast snapshot state, got %+v", st)
			}
			if degraded, _ := st.Meta["degraded"].(bool); !degraded {
				t.Fatalf("expected degraded meta, got %v", st.Meta)
			}
			reason, _ := st.Meta["degraded_reason"].(string)
			if !strings.Contains(reason, tc.reason) {
				t.Fatalf("degraded_reason = %q, want %q", reason, tc.reason)
			}

			initial := r.countCalls(tc.command, tc.argSubstrings...)
			m.refreshStatusCache(context.Background())
			if got := r.countCalls(tc.command, tc.argSubstrings...); got != initial {
				t.Fatalf("refresh during backoff retried %s: before=%d after=%d calls=%#v", tc.command, initial, got, r.calls)
			}

			m.statusMu.RLock()
			cached := m.statusCache
			backoffUntil := m.statusEnrichmentBackoffUntil
			lastErr := m.statusLastEnrichmentErr
			m.statusMu.RUnlock()
			if backoffUntil.IsZero() {
				t.Fatal("quick enrichment failure should set enrichment backoff")
			}
			if !strings.Contains(lastErr, tc.reason) {
				t.Fatalf("last enrichment error = %q, want %q", lastErr, tc.reason)
			}
			if backoff, _ := cached.Meta["enrichment_backoff"].(bool); !backoff {
				t.Fatalf("refresh during backoff should publish backoff metadata, got %v", cached.Meta)
			}
		})
	}
}

func TestStatusEnrichmentCoordinatorSingleFlight(t *testing.T) {
	m, err := newMicroOSBackend(
		WithRunner(fakeRunner{}),
		WithStateDir(t.TempDir()),
		WithRuntimeDir(filepath.Join(t.TempDir(), "run")),
		WithSupportOverride(true),
	)
	if err != nil {
		t.Fatalf("backend: %v", err)
	}

	if ok, _ := m.tryBeginStatusEnrichment(time.Now()); !ok {
		t.Fatal("first enrichment begin should succeed")
	}
	if ok, fields := m.tryBeginStatusEnrichment(time.Now()); ok {
		t.Fatal("second enrichment begin should be denied")
	} else if refreshing, _ := fields["refreshing"].(bool); !refreshing {
		t.Fatalf("expected refreshing metadata, got %v", fields)
	}
	m.finishStatusEnrichment(nil)
}

func TestRefreshStatusCachePublishesFastCoreDuringEnrichmentBackoff(t *testing.T) {
	r := &validationRunner{activeID: "5", defaultID: "7"}
	m, err := newMicroOSBackend(
		WithRunner(r),
		WithStateDir(t.TempDir()),
		WithRuntimeDir(filepath.Join(t.TempDir(), "run")),
		WithSupportOverride(true),
		WithCurrentVersion("v-test"),
		WithReadFile(func(string) ([]byte, error) { return nil, os.ErrNotExist }),
	)
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	m.statusMu.Lock()
	m.statusEnrichmentBackoffUntil = time.Now().Add(time.Minute)
	m.statusLastEnrichmentErr = "snapper timed out"
	m.statusMu.Unlock()

	m.refreshStatusCache(context.Background())

	m.statusMu.RLock()
	st := m.statusCache
	cachedAt := m.statusCachedAt
	m.statusMu.RUnlock()
	if cachedAt.IsZero() {
		t.Fatal("refreshStatusCache should publish fast-core status during enrichment backoff")
	}
	if !st.Pending || !st.RequiresReboot {
		t.Fatalf("expected pending/requires_reboot from fast snapshot state, got %+v", st)
	}
	if backoff, _ := st.Meta["enrichment_backoff"].(bool); !backoff {
		t.Fatalf("expected enrichment_backoff meta, got %v", st.Meta)
	}
	if reason, _ := st.Meta["degraded_reason"].(string); reason != "snapper timed out" {
		t.Fatalf("degraded_reason = %q, want snapper timed out", reason)
	}
	if r.hasCall("snapper") || r.hasCall("zypper") || r.hasCall("rpm") {
		t.Fatalf("refresh during backoff should not call enrichment tools: %#v", r.calls)
	}
}

func TestRefreshStatusCacheFastCoreSkipsInvalidatedCache(t *testing.T) {
	cases := []struct {
		name       string
		setup      func(*microOSBackend)
		assertMeta func(*testing.T, map[string]interface{})
	}{
		{
			name: "backoff",
			setup: func(m *microOSBackend) {
				m.statusEnrichmentBackoffUntil = time.Now().Add(time.Minute)
				m.statusLastEnrichmentErr = "snapper timed out"
			},
			assertMeta: func(t *testing.T, meta map[string]interface{}) {
				t.Helper()
				if backoff, _ := meta["enrichment_backoff"].(bool); !backoff {
					t.Fatalf("expected enrichment_backoff meta, got %v", meta)
				}
				if reason, _ := meta["degraded_reason"].(string); reason != "snapper timed out" {
					t.Fatalf("degraded_reason = %q, want snapper timed out", reason)
				}
			},
		},
		{
			name: "active",
			setup: func(m *microOSBackend) {
				m.statusRefreshActive = true
			},
			assertMeta: func(t *testing.T, meta map[string]interface{}) {
				t.Helper()
				if refreshing, _ := meta["refreshing"].(bool); !refreshing {
					t.Fatalf("expected refreshing meta, got %v", meta)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &validationRunner{activeID: "7", defaultID: "7"}
			m, err := newMicroOSBackend(
				WithRunner(r),
				WithStateDir(t.TempDir()),
				WithRuntimeDir(filepath.Join(t.TempDir(), "run")),
				WithSupportOverride(true),
				WithCurrentVersion("v-current"),
				WithReadFile(func(string) ([]byte, error) { return nil, os.ErrNotExist }),
			)
			if err != nil {
				t.Fatalf("backend: %v", err)
			}

			invalidatedAt := time.Now()
			m.statusMu.Lock()
			m.statusCache = Status{
				CurrentVersion:   "v-old",
				AvailableVersion: "stale-available",
				Pending:          true,
				RequiresReboot:   true,
				LastChecked:      time.Unix(100, 0),
				Meta: map[string]interface{}{
					"last_request":    persistedState{LastAction: "apply", TargetSnapshot: "42", RequestedAt: time.Unix(99, 0)},
					"piccolod_staged": "stale-rpm",
				},
			}
			m.statusCachedAt = invalidatedAt.Add(-time.Minute)
			m.statusInvalidatedAt = invalidatedAt
			tc.setup(m)
			m.statusMu.Unlock()

			m.refreshStatusCache(context.Background())

			m.statusMu.RLock()
			st := m.statusCache
			cachedAt := m.statusCachedAt
			m.statusMu.RUnlock()

			if !cachedAt.After(invalidatedAt) {
				t.Fatalf("refresh did not publish a post-invalidation sample: cachedAt=%v invalidatedAt=%v", cachedAt, invalidatedAt)
			}
			if st.CurrentVersion != "v-current" || st.AvailableVersion != "v-current" {
				t.Fatalf("fast-core refresh reused stale version fields: %+v", st)
			}
			if st.Pending || st.RequiresReboot {
				t.Fatalf("fast-core refresh reused stale reboot flags: %+v", st)
			}
			if _, ok := st.Meta["last_request"]; ok {
				t.Fatalf("fast-core refresh reused stale last_request metadata: %v", st.Meta)
			}
			if _, ok := st.Meta["piccolod_staged"]; ok {
				t.Fatalf("fast-core refresh reused stale staged RPM metadata: %v", st.Meta)
			}
			if got := st.Meta["snapshot_readiness"]; got != string(SnapshotReadinessAbsent) {
				t.Fatalf("snapshot_readiness = %v, want absent; meta=%v", got, st.Meta)
			}
			tc.assertMeta(t, st.Meta)
			if r.hasCall("snapper") || r.hasCall("zypper") || r.hasCall("rpm") {
				t.Fatalf("fast-core refresh should not call enrichment tools: %#v", r.calls)
			}
		})
	}
}

func TestRefreshStatusCacheInProgressDoesNotEnterEnrichmentBackoff(t *testing.T) {
	r := &validationRunner{activeID: "5", defaultID: "7", inProgress: true}
	m, err := newMicroOSBackend(
		WithRunner(r),
		WithStateDir(t.TempDir()),
		WithRuntimeDir(filepath.Join(t.TempDir(), "run")),
		WithSupportOverride(true),
	)
	if err != nil {
		t.Fatalf("backend: %v", err)
	}

	m.refreshStatusCache(context.Background())

	m.statusMu.RLock()
	backoffUntil := m.statusEnrichmentBackoffUntil
	cachedAt := m.statusCachedAt
	m.statusMu.RUnlock()
	if !backoffUntil.IsZero() {
		t.Fatalf("in-progress fast state should not set enrichment backoff: %v", backoffUntil)
	}
	if !cachedAt.IsZero() {
		t.Fatalf("in-progress fast state should not publish a 200 status cache")
	}
}

type targetCaptureRunner struct {
	validationRunner
}

func (r *targetCaptureRunner) Run(ctx context.Context, name string, args ...string) (string, string, int, error) {
	r.calls = append(r.calls, call{name: name, args: append([]string{}, args...)})
	switch name {
	case "systemd-run":
		return "started", "", 0, nil
	case "systemctl":
		if len(args) > 0 && args[0] == "list-units" {
			return "", "", 0, nil
		}
		if len(args) > 0 && args[0] == "is-active" {
			return "", "", 3, nil
		}
	case "findmnt":
		return "/dev/sda3[/@/.snapshots/5/snapshot]\n", "", 0, nil
	case "btrfs":
		if len(args) >= 2 && args[0] == "subvolume" && args[1] == "get-default" {
			return "ID 7 gen 59 top level 257 path @/.snapshots/7/snapshot", "", 0, nil
		}
	}
	return "", "", 0, nil
}

func TestRunTransactionalUpdateCapturesTargetWithoutFullStatus(t *testing.T) {
	tmp := t.TempDir()
	r := &targetCaptureRunner{}
	m, err := newMicroOSBackend(
		WithRunner(r),
		WithClock(func() time.Time { return time.Unix(1700000000, 0) }),
		WithStateDir(tmp),
		WithRuntimeDir(filepath.Join(tmp, "run")),
		WithSupportOverride(true),
		WithFreeBytesFn(func(string) (uint64, error) { return 10 << 30, nil }),
	)
	if err != nil {
		t.Fatalf("backend: %v", err)
	}

	if err := m.runTransactionalUpdate(context.Background(), []string{"transactional-update", "dup"}, "apply", "", false); err != nil {
		t.Fatalf("runTransactionalUpdate: %v", err)
	}
	if r.hasCall("snapper") || r.hasCall("zypper") || r.hasCall("rpm") {
		t.Fatalf("target capture should not call full status/enrichment tools: %#v", r.calls)
	}
	ps := m.loadState()
	if ps == nil || ps.TargetSnapshot != "7" {
		t.Fatalf("persisted target = %+v, want snapshot 7", ps)
	}
	if _, err := os.Stat(filepath.Join(tmp, "run", "update.inprogress")); err != nil {
		t.Fatalf("async target capture should leave in-progress marker intact: %v", err)
	}
}

// createSnapshotDir creates a snapshot directory structure with the specified files present.
func createSnapshotDir(t *testing.T, baseDir, snapshotID string, files []string) {
	t.Helper()
	root := filepath.Join(baseDir, snapshotID, "snapshot")
	for _, f := range files {
		path := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte("binary"), 0o755); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
}

// allCriticalFiles returns the full set of files needed for snapshot validation to pass.
func allCriticalFiles() []string {
	return []string{
		"usr/lib/systemd/systemd",
		"usr/sbin/cryptsetup",
		"usr/sbin/ip",
		"usr/lib/modules/6.1.0/vmlinuz",
	}
}

func TestReboot(t *testing.T) {
	t.Run("valid_staged_snapshot", func(t *testing.T) {
		tmp := t.TempDir()
		snapDir := filepath.Join(tmp, "snapshots")
		createSnapshotDir(t, snapDir, "7", allCriticalFiles())

		r := &validationRunner{activeID: "5", defaultID: "7"}
		m, err := newMicroOSBackend(
			WithRunner(r),
			WithStateDir(tmp),
			WithRuntimeDir(filepath.Join(tmp, "run")),
			WithSupportOverride(true),
			WithSnapshotsDir(snapDir),
		)
		if err != nil {
			t.Fatalf("backend: %v", err)
		}

		if err := m.Reboot(context.Background()); err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if !r.hasCall("systemctl", "reboot") {
			t.Fatal("expected systemctl reboot to be called")
		}
	})

	t.Run("missing_kernel_blocks_reboot", func(t *testing.T) {
		tmp := t.TempDir()
		snapDir := filepath.Join(tmp, "snapshots")
		// Create files without kernel
		createSnapshotDir(t, snapDir, "7", []string{
			"usr/lib/systemd/systemd",
			"usr/sbin/cryptsetup",
			"usr/sbin/ip",
		})

		r := &validationRunner{activeID: "5", defaultID: "7"}
		m, err := newMicroOSBackend(
			WithRunner(r),
			WithStateDir(tmp),
			WithRuntimeDir(filepath.Join(tmp, "run")),
			WithSupportOverride(true),
			WithSnapshotsDir(snapDir),
		)
		if err != nil {
			t.Fatalf("backend: %v", err)
		}

		err = m.Reboot(context.Background())
		if !errors.Is(err, ErrSnapshotValidationFailed) {
			t.Fatalf("expected ErrSnapshotValidationFailed, got %v", err)
		}
		if r.hasCall("systemctl", "reboot") {
			t.Fatal("reboot should not have been called")
		}
		if !r.hasCall("snapper", "modify", "--default") {
			t.Fatal("expected snapper modify --default to revert")
		}
		if !r.hasCall("snapper", "delete", "7") {
			t.Fatal("expected snapper delete of bad snapshot")
		}
	})

	t.Run("missing_systemd_blocks_reboot", func(t *testing.T) {
		tmp := t.TempDir()
		snapDir := filepath.Join(tmp, "snapshots")
		createSnapshotDir(t, snapDir, "7", []string{
			"usr/sbin/cryptsetup",
			"usr/sbin/ip",
			"usr/lib/modules/6.1.0/vmlinuz",
		})

		r := &validationRunner{activeID: "5", defaultID: "7"}
		m, err := newMicroOSBackend(
			WithRunner(r),
			WithStateDir(tmp),
			WithRuntimeDir(filepath.Join(tmp, "run")),
			WithSupportOverride(true),
			WithSnapshotsDir(snapDir),
		)
		if err != nil {
			t.Fatalf("backend: %v", err)
		}

		err = m.Reboot(context.Background())
		if !errors.Is(err, ErrSnapshotValidationFailed) {
			t.Fatalf("expected ErrSnapshotValidationFailed, got %v", err)
		}
		if r.hasCall("systemctl", "reboot") {
			t.Fatal("reboot should not have been called")
		}
	})

	t.Run("no_staged_snapshot_passes", func(t *testing.T) {
		tmp := t.TempDir()
		r := &validationRunner{activeID: "5", defaultID: "5"}
		m, err := newMicroOSBackend(
			WithRunner(r),
			WithStateDir(tmp),
			WithRuntimeDir(filepath.Join(tmp, "run")),
			WithSupportOverride(true),
			WithSnapshotsDir(filepath.Join(tmp, "snapshots")),
		)
		if err != nil {
			t.Fatalf("backend: %v", err)
		}

		if err := m.Reboot(context.Background()); err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if !r.hasCall("systemctl", "reboot") {
			t.Fatal("expected reboot to proceed")
		}
	})

	t.Run("force_bypasses_validation", func(t *testing.T) {
		tmp := t.TempDir()
		// Don't create any snapshot files — validation would fail
		r := &validationRunner{activeID: "5", defaultID: "7"}
		m, err := newMicroOSBackend(
			WithRunner(r),
			WithStateDir(tmp),
			WithRuntimeDir(filepath.Join(tmp, "run")),
			WithSupportOverride(true),
			WithSnapshotsDir(filepath.Join(tmp, "snapshots")),
		)
		if err != nil {
			t.Fatalf("backend: %v", err)
		}

		if err := m.ForceReboot(context.Background()); err != nil {
			t.Fatalf("expected no error from ForceReboot, got %v", err)
		}
		if !r.hasCall("systemctl", "reboot") {
			t.Fatal("expected reboot to proceed")
		}
	})

	t.Run("empty_default_fails_closed", func(t *testing.T) {
		tmp := t.TempDir()
		r := &validationRunner{activeID: "5", defaultID: ""}
		m, err := newMicroOSBackend(
			WithRunner(r),
			WithStateDir(tmp),
			WithRuntimeDir(filepath.Join(tmp, "run")),
			WithSupportOverride(true),
			WithSnapshotsDir(filepath.Join(tmp, "snapshots")),
		)
		if err != nil {
			t.Fatalf("backend: %v", err)
		}

		err = m.Reboot(context.Background())
		if err == nil {
			t.Fatal("expected error for empty default (fail-closed)")
		}
		// Lookup failure should NOT wrap ErrSnapshotValidationFailed —
		// that sentinel is reserved for confirmed content failures.
		if errors.Is(err, ErrSnapshotValidationFailed) {
			t.Fatal("lookup failure should not be ErrSnapshotValidationFailed")
		}
		if r.hasCall("systemctl", "reboot") {
			t.Fatal("reboot should not have been called")
		}
		// Lookup failure should NOT trigger destructive cleanup
		if r.hasCall("snapper", "modify") || r.hasCall("snapper", "delete") {
			t.Fatal("lookup failure should not trigger revert/delete")
		}
	})

	t.Run("revert_failure_skips_delete", func(t *testing.T) {
		tmp := t.TempDir()
		// Don't create any snapshot files — validation will fail
		r := &validationRunner{activeID: "5", defaultID: "7", revertFails: true}
		m, err := newMicroOSBackend(
			WithRunner(r),
			WithStateDir(tmp),
			WithRuntimeDir(filepath.Join(tmp, "run")),
			WithSupportOverride(true),
			WithSnapshotsDir(filepath.Join(tmp, "snapshots")),
		)
		if err != nil {
			t.Fatalf("backend: %v", err)
		}

		err = m.Reboot(context.Background())
		if !errors.Is(err, ErrSnapshotValidationFailed) {
			t.Fatalf("expected error wrapping ErrSnapshotValidationFailed, got %v", err)
		}
		if r.hasCall("snapper", "delete") {
			t.Fatal("should not delete snapshot when revert fails")
		}
		if r.hasCall("systemctl", "reboot") {
			t.Fatal("reboot should not have been called")
		}
	})
}

func TestWatchSnapshots(t *testing.T) {
	t.Run("reverts_bad_snapshot", func(t *testing.T) {
		tmp := t.TempDir()
		snapDir := filepath.Join(tmp, "snapshots")
		// Create empty snapshot dir (no critical files)
		if err := os.MkdirAll(filepath.Join(snapDir, "7", "snapshot"), 0o755); err != nil {
			t.Fatal(err)
		}

		r := &validationRunner{activeID: "5", defaultID: "7"}
		m, err := newMicroOSBackend(
			WithRunner(r),
			WithStateDir(tmp),
			WithRuntimeDir(filepath.Join(tmp, "run")),
			WithSupportOverride(true),
			WithSnapshotsDir(snapDir),
		)
		if err != nil {
			t.Fatalf("backend: %v", err)
		}

		m.watchSnapshots(context.Background())

		if !r.hasCall("snapper", "modify", "--default") {
			t.Fatal("expected revert via snapper modify --default")
		}
		if !r.hasCall("snapper", "delete", "7") {
			t.Fatal("expected snapper delete of bad snapshot")
		}
	})

	t.Run("skips_when_in_progress", func(t *testing.T) {
		tmp := t.TempDir()
		r := &validationRunner{activeID: "5", defaultID: "7", inProgress: true}
		m, err := newMicroOSBackend(
			WithRunner(r),
			WithStateDir(tmp),
			WithRuntimeDir(filepath.Join(tmp, "run")),
			WithSupportOverride(true),
			WithSnapshotsDir(filepath.Join(tmp, "snapshots")),
		)
		if err != nil {
			t.Fatalf("backend: %v", err)
		}

		m.watchSnapshots(context.Background())

		if r.hasCall("snapper", "modify") {
			t.Fatal("should not revert while TU is in progress")
		}
	})

	t.Run("no_staged_no_action", func(t *testing.T) {
		tmp := t.TempDir()
		r := &validationRunner{activeID: "5", defaultID: "5"}
		m, err := newMicroOSBackend(
			WithRunner(r),
			WithStateDir(tmp),
			WithRuntimeDir(filepath.Join(tmp, "run")),
			WithSupportOverride(true),
			WithSnapshotsDir(filepath.Join(tmp, "snapshots")),
		)
		if err != nil {
			t.Fatalf("backend: %v", err)
		}

		m.watchSnapshots(context.Background())

		if r.hasCall("snapper", "modify") || r.hasCall("snapper", "delete") {
			t.Fatal("should not take action when no staged snapshot")
		}
	})
}

func TestRollbackInvalidSnapshotReturnsError(t *testing.T) {
	m, err := newMicroOSBackend(
		WithRunner(snapshotRunner{}),
		WithSupportOverride(true),
		WithStateDir(t.TempDir()),
		WithRuntimeDir(filepath.Join(t.TempDir(), "run")),
	)
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	if err := m.Rollback(context.Background(), "999"); err != ErrInvalidSnapshot {
		t.Fatalf("expected ErrInvalidSnapshot, got %v", err)
	}
}

// countingRunner wraps fakeRunner and counts shellouts so tests can verify
// status caching avoids re-running readStatus.
type countingRunner struct {
	fakeRunner
	calls atomic.Int32
}

func (r *countingRunner) Run(ctx context.Context, name string, args ...string) (string, string, int, error) {
	r.calls.Add(1)
	return r.fakeRunner.Run(ctx, name, args...)
}

// expectedFastStatusShellouts caps the runner-call overhead a Status() call
// pays even when served from cache: the fast snapshot probe checks in-progress
// state plus active/default roots. If SnapshotState grows new probes, this
// needs to follow.
const expectedFastStatusShellouts = 4

func TestStatusServedFromSnapshotCache(t *testing.T) {
	tmp := t.TempDir()
	r := &countingRunner{}
	m, err := newMicroOSBackend(
		WithRunner(r),
		WithStateDir(tmp),
		WithRuntimeDir(filepath.Join(tmp, "run")),
		WithSupportOverride(true),
		WithReadFile(func(string) ([]byte, error) { return nil, os.ErrNotExist }),
	)
	if err != nil {
		t.Fatalf("backend: %v", err)
	}

	if _, err := m.Status(context.Background()); err != nil {
		t.Fatalf("first Status: %v", err)
	}
	primingDelta := r.calls.Load()
	if primingDelta <= expectedFastStatusShellouts {
		t.Fatalf("expected first Status to invoke many runner calls; got %d", primingDelta)
	}

	if _, err := m.Status(context.Background()); err != nil {
		t.Fatalf("second Status: %v", err)
	}
	cachedDelta := r.calls.Load() - primingDelta
	if cachedDelta > expectedFastStatusShellouts {
		t.Errorf("cached Status made %d runner calls; expected ≤%d (fast snapshot overhead)", cachedDelta, expectedFastStatusShellouts)
	}

	m.invalidateStatusCache()
	if st, err := m.Status(context.Background()); err != nil {
		t.Fatalf("post-invalidate Status: %v", err)
	} else if stale, _ := st.Meta["stale"].(bool); !stale {
		t.Fatalf("post-invalidate Status should serve stale cache while refreshing, got meta=%v", st.Meta)
	}
}

// TestStatusInvalidateDuringRefresh asserts that a sample taken before an
// invalidate is NOT published as fresh after the invalidate. Stale cache may
// still be returned to callers, but it must be explicitly marked stale.
func TestStatusInvalidateDuringRefresh(t *testing.T) {
	tmp := t.TempDir()
	m, err := newMicroOSBackend(
		WithRunner(fakeRunner{}),
		WithStateDir(tmp),
		WithRuntimeDir(filepath.Join(tmp, "run")),
		WithSupportOverride(true),
		WithReadFile(func(string) ([]byte, error) { return nil, os.ErrNotExist }),
	)
	if err != nil {
		t.Fatalf("backend: %v", err)
	}

	// Simulate a sample-start preceding the invalidation.
	sampleStart := time.Now()
	time.Sleep(2 * time.Millisecond) // ensure invalidatedAt > sampleStart
	m.invalidateStatusCache()

	// Attempt to publish a snapshot using the pre-invalidate sampleStart;
	// the guard must drop the write.
	m.publishStatusSnapshot(Status{CurrentVersion: "stale"}, sampleStart)

	m.statusMu.RLock()
	cachedAt := m.statusCachedAt
	cachedVer := m.statusCache.CurrentVersion
	m.statusMu.RUnlock()
	if !cachedAt.IsZero() {
		t.Errorf("publish-after-invalidate succeeded: cachedAt=%v want zero", cachedAt)
	}
	if cachedVer == "stale" {
		t.Errorf("publish-after-invalidate published stale snapshot: %q", cachedVer)
	}

	// A sample taken *after* the invalidate must publish normally.
	freshSample := time.Now()
	m.publishStatusSnapshot(Status{CurrentVersion: "fresh"}, freshSample)
	m.statusMu.RLock()
	cachedVer = m.statusCache.CurrentVersion
	m.statusMu.RUnlock()
	if cachedVer != "fresh" {
		t.Errorf("post-invalidate publish failed: cachedVer=%q want fresh", cachedVer)
	}
}

func TestApplyInvalidatesStatusCache(t *testing.T) {
	tmp := t.TempDir()
	r := &countingRunner{}
	m, err := NewManager(
		WithRunner(r),
		WithStateDir(tmp),
		WithRuntimeDir(filepath.Join(tmp, "run")),
		WithSupportOverride(true),
		WithReadFile(func(string) ([]byte, error) { return nil, os.ErrNotExist }),
		WithTimeout(50*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("manager: %v", err)
	}

	// Prime the cache.
	if _, err := m.Status(context.Background()); err != nil {
		t.Fatalf("prime Status: %v", err)
	}
	primingCalls := r.calls.Load()

	if _, err := m.Status(context.Background()); err != nil {
		t.Fatalf("cached Status: %v", err)
	}
	if delta := r.calls.Load() - primingCalls; delta > expectedFastStatusShellouts {
		t.Fatalf("cache not primed: cached call made %d runner calls", delta)
	}

	// Apply will fail (fakeRunner doesn't implement systemd-run for TU), but the
	// invalidation hook in the Manager facade fires via defer regardless.
	_ = m.Apply(context.Background())
	if st, err := m.Status(context.Background()); err != nil {
		t.Fatalf("post-apply Status: %v", err)
	} else if stale, _ := st.Meta["stale"].(bool); !stale {
		t.Fatalf("post-Apply Status should serve stale cache while refreshing, got meta=%v", st.Meta)
	}
}

type hangingStatusRunner struct{}

func (hangingStatusRunner) Run(ctx context.Context, name string, args ...string) (string, string, int, error) {
	switch name {
	case "systemctl":
		if len(args) > 0 && args[0] == "list-units" {
			return "", "", 0, nil
		}
		if len(args) > 0 && args[0] == "is-active" {
			return "", "", 3, nil
		}
	}
	<-ctx.Done()
	return "", "", -1, ctx.Err()
}

type hangingInProgressRunner struct {
	enabled atomic.Bool
	hangs   atomic.Int32
}

func (r *hangingInProgressRunner) Run(ctx context.Context, name string, args ...string) (string, string, int, error) {
	if name == "systemctl" {
		if len(args) > 0 && (args[0] == "list-units" || args[0] == "is-active") {
			if !r.enabled.Load() {
				return "", "", 3, nil
			}
			if r.hangs.Add(1) == 1 {
				<-ctx.Done()
				return "", "", -1, ctx.Err()
			}
			return "", "", 3, nil
		}
	}
	select {
	case <-ctx.Done():
		return "", "", -1, ctx.Err()
	default:
		return "", "", 0, nil
	}
}

func TestStatusDegradesWhenInitialProbeTimesOut(t *testing.T) {
	tmp := t.TempDir()
	m, err := newMicroOSBackend(
		WithRunner(hangingStatusRunner{}),
		WithStateDir(tmp),
		WithRuntimeDir(filepath.Join(tmp, "run")),
		WithSupportOverride(true),
		WithCurrentVersion("v-test"),
		WithReadFile(func(string) ([]byte, error) { return nil, os.ErrNotExist }),
		WithStatusRequestTimeout(10*time.Millisecond),
		WithStatusRefreshTimeout(10*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("backend: %v", err)
	}

	start := time.Now()
	st, err := m.Status(context.Background())
	if err != nil {
		t.Fatalf("Status should degrade instead of erroring: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("Status took %v; expected bounded degraded response", elapsed)
	}
	if st.CurrentVersion != "v-test" || st.AvailableVersion != "v-test" {
		t.Fatalf("unexpected degraded versions: %+v", st)
	}
	if degraded, _ := st.Meta["degraded"].(bool); !degraded {
		t.Fatalf("expected degraded meta, got %v", st.Meta)
	}
	if empty, _ := st.Meta["cache_empty"].(bool); !empty {
		t.Fatalf("expected cache_empty meta, got %v", st.Meta)
	}
}

func TestStatusDegradesWhenInProgressProbeTimesOut(t *testing.T) {
	tmp := t.TempDir()
	r := &hangingInProgressRunner{}
	m, err := newMicroOSBackend(
		WithRunner(r),
		WithStateDir(tmp),
		WithRuntimeDir(filepath.Join(tmp, "run")),
		WithSupportOverride(true),
		WithCurrentVersion("v-test"),
		WithReadFile(func(string) ([]byte, error) { return nil, os.ErrNotExist }),
		WithStatusRequestTimeout(10*time.Millisecond),
		WithStatusRefreshTimeout(10*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	r.enabled.Store(true)

	start := time.Now()
	st, err := m.Status(context.Background())
	if err != nil {
		t.Fatalf("Status should degrade instead of erroring: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("Status took %v; expected bounded degraded response", elapsed)
	}
	if degraded, _ := st.Meta["degraded"].(bool); !degraded {
		t.Fatalf("expected degraded meta, got %v", st.Meta)
	}
	if empty, _ := st.Meta["cache_empty"].(bool); !empty {
		t.Fatalf("expected cache_empty meta, got %v", st.Meta)
	}
}

func TestStatusInProgressTimeoutDoesNotRemoveMarker(t *testing.T) {
	tmp := t.TempDir()
	runDir := filepath.Join(tmp, "run")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	marker := filepath.Join(runDir, "update.inprogress")
	r := &hangingInProgressRunner{}
	m, err := newMicroOSBackend(
		WithRunner(r),
		WithStateDir(tmp),
		WithRuntimeDir(runDir),
		WithSupportOverride(true),
		WithCurrentVersion("v-test"),
		WithReadFile(func(string) ([]byte, error) { return nil, os.ErrNotExist }),
		WithStatusRequestTimeout(10*time.Millisecond),
		WithStatusRefreshTimeout(10*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	if err := os.WriteFile(marker, []byte("apply piccolo-tu-test.service"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	r.enabled.Store(true)

	if _, err := m.Status(context.Background()); err != nil {
		t.Fatalf("Status should degrade instead of erroring: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("in-progress marker should remain after timeout, got %v", err)
	}
}
