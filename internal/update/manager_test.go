package update

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStatusWithStagedSnapshot(t *testing.T) {
	tmp := t.TempDir()
	clock := func() time.Time { return time.Date(2025, 11, 24, 10, 0, 0, 0, time.UTC) }
	m, err := NewManager(
		WithRunner(fakeRunner{}),
		WithStateDir(tmp),
		WithRuntimeDir(filepath.Join(tmp, "run")),
		WithClock(clock),
		WithSupportOverride(true),
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
	if st.CurrentVersion == st.AvailableVersion {
		t.Fatalf("expected different available version when staged")
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
		return "success\n0\nMon 2025-11-24 09:59:00 UTC", "", 0, nil
	case "journalctl":
		return "txn ok\nprepared snapshot 7\n", "", 0, nil
	case "zypper":
		return "<update/><update/>", "", 0, nil
	case "rpm":
		// Distinguish staged vs active via --root flag
		for i, a := range args {
			if a == "--root" && i+1 < len(args) && args[i+1] != "" {
				return "piccolod-1.2.4\n", "", 0, nil
			}
		}
		return "piccolod-1.2.3\n", "", 0, nil
	default:
		return "", "", 0, nil
	}
}
