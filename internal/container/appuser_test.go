package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// mockResolver implements CredentialResolver for testing.
type mockResolver struct {
	users  map[string]*user.User
	groups map[string]*user.Group
}

func (m *mockResolver) LookupUser(username string) (*user.User, error) {
	if u, ok := m.users[username]; ok {
		return u, nil
	}
	return nil, fmt.Errorf("user: unknown user %s", username)
}

func (m *mockResolver) LookupGroup(groupName string) (*user.Group, error) {
	if g, ok := m.groups[groupName]; ok {
		return g, nil
	}
	return nil, fmt.Errorf("group: unknown group %s", groupName)
}

// mockExecutor implements SystemExecutor for testing.
type mockExecutor struct {
	// calls records all (command, args) invocations for assertion.
	calls [][]string
	// results maps "command arg1 arg2..." to (output, error).
	results map[string]mockResult
	// defaultResult is returned when no specific result is configured.
	defaultResult mockResult
	// onRun is an optional callback invoked before returning from Run.
	// Tests use this to simulate side effects (e.g., useradd making a user visible).
	onRun func(name string, args ...string)
	// blockUntilContext maps commands that should simulate a hung subprocess.
	blockUntilContext map[string]bool
}

type mockResult struct {
	output []byte
	err    error
}

func (m *mockExecutor) Run(name string, args ...string) ([]byte, error) {
	call := append([]string{name}, args...)
	m.calls = append(m.calls, call)
	if m.onRun != nil {
		m.onRun(name, args...)
	}
	key := strings.Join(call, " ")
	if r, ok := m.results[key]; ok {
		return r.output, r.err
	}
	return m.defaultResult.output, m.defaultResult.err
}

func (m *mockExecutor) RunContext(ctx context.Context, name string, args ...string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	call := append([]string{name}, args...)
	key := strings.Join(call, " ")
	if m.blockUntilContext[key] {
		m.calls = append(m.calls, call)
		if m.onRun != nil {
			m.onRun(name, args...)
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return m.Run(name, args...)
}

func systemUnitShowKey(unit string) string {
	return fmt.Sprintf("systemctl show %s --property=ActiveState --property=SubState --property=Result --property=ControlGroup --no-pager", unit)
}

func userSessionShowKey(uid uint32) string {
	return systemUnitShowKey(fmt.Sprintf("user@%d.service", uid))
}

func userBusProbeKey(username string, uid uint32) string {
	return fmt.Sprintf("/usr/sbin/runuser --user %s -- /usr/bin/env XDG_RUNTIME_DIR=/run/user/%d DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/%d/bus /usr/bin/busctl --user --no-pager --quiet list", username, uid, uid)
}

func hasExecutorCall(calls [][]string, want ...string) bool {
	for _, call := range calls {
		if strings.Join(call, "\x00") == strings.Join(want, "\x00") {
			return true
		}
	}
	return false
}

func TestEnsureUserSessionRepairsFailedUnitDespiteStaleBusPath(t *testing.T) {
	oldExecutor := defaultExecutor
	defer func() { defaultExecutor = oldExecutor }()

	const uid = uint32(475)
	const username = "pa-namek"
	exec := &mockExecutor{results: map[string]mockResult{}}
	exec.results[userSessionShowKey(uid)] = mockResult{output: []byte("ActiveState=failed\nSubState=failed\nResult=signal\nControlGroup=/user.slice/user-475.slice/user@475.service\n")}
	exec.onRun = func(name string, args ...string) {
		if name == "systemctl" && len(args) >= 2 && args[0] == "start" {
			exec.results[userSessionShowKey(uid)] = mockResult{output: []byte("ActiveState=active\nSubState=running\nResult=success\nControlGroup=/user.slice/user-475.slice/user@475.service\n")}
			exec.results[userBusProbeKey(username, uid)] = mockResult{}
		}
	}
	defaultExecutor = exec

	if err := ensureUserSession(context.Background(), "namek", username, uid); err != nil {
		t.Fatalf("ensureUserSession: %v", err)
	}
	if !hasExecutorCall(exec.calls, "systemctl", "start", "user@475.service") {
		t.Fatalf("expected failed unit to be started, calls=%v", exec.calls)
	}
	if !hasExecutorCall(exec.calls, "/usr/sbin/runuser", "--user", username, "--", "/usr/bin/env",
		"XDG_RUNTIME_DIR=/run/user/475", "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/475/bus",
		"/usr/bin/busctl", "--user", "--no-pager", "--quiet", "list") {
		t.Fatalf("expected real user-bus probe, calls=%v", exec.calls)
	}
}

func TestObserveUserSessionDoesNotRepairActiveButUnusableBus(t *testing.T) {
	oldExecutor := defaultExecutor
	defer func() { defaultExecutor = oldExecutor }()

	const uid = uint32(475)
	const username = "pa-namek"
	exec := &mockExecutor{results: map[string]mockResult{
		userSessionShowKey(uid):        {output: []byte("ActiveState=active\nSubState=running\nResult=success\nControlGroup=/user.slice/user-475.slice/user@475.service\n")},
		userBusProbeKey(username, uid): {output: []byte("connection refused"), err: errors.New("exit status 1")},
	}}
	defaultExecutor = exec

	err := waitForUserSession(context.Background(), "namek", username, uid, false)
	if !errors.Is(err, ErrUserSessionUnavailable) {
		t.Fatalf("expected ErrUserSessionUnavailable, got %v", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("observe-only bus failure waited for the repair deadline: %v", err)
	}
	if hasExecutorCall(exec.calls, "systemctl", "restart", "user@475.service") {
		t.Fatalf("observe-only path restarted the unit, calls=%v", exec.calls)
	}
}

func TestEnsureUserSessionRestartsActiveButUnusableBus(t *testing.T) {
	oldExecutor := defaultExecutor
	defer func() { defaultExecutor = oldExecutor }()

	const uid = uint32(475)
	const username = "pa-namek"
	exec := &mockExecutor{results: map[string]mockResult{
		userSessionShowKey(uid):        {output: []byte("ActiveState=active\nSubState=running\nResult=success\nControlGroup=/user.slice/user-475.slice/user@475.service\n")},
		userBusProbeKey(username, uid): {output: []byte("connection refused"), err: errors.New("exit status 1")},
	}}
	exec.onRun = func(name string, args ...string) {
		if name == "systemctl" && len(args) >= 2 && args[0] == "restart" {
			exec.results[userBusProbeKey(username, uid)] = mockResult{}
		}
	}
	defaultExecutor = exec

	if err := ensureUserSession(context.Background(), "namek", username, uid); err != nil {
		t.Fatalf("ensureUserSession: %v", err)
	}
	if !hasExecutorCall(exec.calls, "systemctl", "restart", "user@475.service") {
		t.Fatalf("expected active unusable unit to be restarted, calls=%v", exec.calls)
	}
}

func TestEnsureUserSessionStartsInactiveUnit(t *testing.T) {
	oldExecutor := defaultExecutor
	defer func() { defaultExecutor = oldExecutor }()

	const uid = uint32(475)
	const username = "pa-namek"
	exec := &mockExecutor{results: map[string]mockResult{
		userSessionShowKey(uid): {output: []byte("ActiveState=inactive\nSubState=dead\nResult=success\nControlGroup=\n")},
	}}
	exec.onRun = func(name string, args ...string) {
		if name == "systemctl" && len(args) >= 2 && args[0] == "start" {
			exec.results[userSessionShowKey(uid)] = mockResult{output: []byte("ActiveState=active\nSubState=running\nResult=success\nControlGroup=/user.slice/user-475.slice/user@475.service\n")}
			exec.results[userBusProbeKey(username, uid)] = mockResult{}
		}
	}
	defaultExecutor = exec

	if err := ensureUserSession(context.Background(), "namek", username, uid); err != nil {
		t.Fatalf("ensureUserSession: %v", err)
	}
	if !hasExecutorCall(exec.calls, "systemctl", "start", "user@475.service") {
		t.Fatalf("inactive unit was not started, calls=%v", exec.calls)
	}
}

func TestUserSessionStateAndRepairFailuresAreContextual(t *testing.T) {
	const uid = uint32(475)
	const username = "pa-namek"
	tests := []struct {
		name           string
		show           mockResult
		bus            mockResult
		action         string
		actionResult   mockResult
		wantPreActive  string
		wantPostActive string
		wantAction     string
		wantResult     string
		wantText       string
	}{
		{
			name:       "query failure",
			show:       mockResult{output: []byte("pid1 unavailable"), err: errors.New("query failed")},
			wantAction: "none", wantResult: "not-attempted", wantText: "query failed",
		},
		{
			name:          "maintenance state",
			show:          mockResult{output: []byte("ActiveState=maintenance\nSubState=maintenance\nResult=signal\nControlGroup=/user.slice/user-475.slice/user@475.service\n")},
			wantPreActive: "maintenance", wantPostActive: "maintenance",
			wantAction: "none", wantResult: "not-attempted", wantText: "unsupported unit state",
		},
		{
			name:   "start failure",
			show:   mockResult{output: []byte("ActiveState=inactive\nSubState=dead\nResult=success\nControlGroup=\n")},
			action: "start", actionResult: mockResult{output: []byte("authorization failed"), err: errors.New("start failed")},
			wantPreActive: "inactive", wantPostActive: "inactive",
			wantAction: "start", wantResult: "failed", wantText: "start failed",
		},
		{
			name:   "restart failure",
			show:   mockResult{output: []byte("ActiveState=active\nSubState=running\nResult=success\nControlGroup=/user.slice/user-475.slice/user@475.service\n")},
			bus:    mockResult{output: []byte("connection refused"), err: errors.New("dead bus")},
			action: "restart", actionResult: mockResult{output: []byte("restart rejected"), err: errors.New("restart failed")},
			wantPreActive: "active", wantPostActive: "active",
			wantAction: "restart", wantResult: "failed", wantText: "restart failed",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			oldExecutor := defaultExecutor
			defer func() { defaultExecutor = oldExecutor }()
			exec := &mockExecutor{results: map[string]mockResult{
				userSessionShowKey(uid):        tc.show,
				userBusProbeKey(username, uid): tc.bus,
			}}
			if tc.action != "" {
				exec.results[fmt.Sprintf("systemctl %s user@475.service", tc.action)] = tc.actionResult
			}
			defaultExecutor = exec

			err := ensureUserSession(context.Background(), "namek", username, uid)
			if err == nil || !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("ensureUserSession error = %v, want %q", err, tc.wantText)
			}
			var unavailable *userSessionUnavailableError
			if !errors.As(err, &unavailable) {
				t.Fatalf("error type = %T, want userSessionUnavailableError", err)
			}
			if unavailable.InstanceID != "namek" || unavailable.UID != uid || unavailable.Unit != "user@475.service" ||
				unavailable.PreActionState.ActiveState != tc.wantPreActive || unavailable.PostActionState.ActiveState != tc.wantPostActive ||
				unavailable.RepairAction != tc.wantAction || unavailable.RepairResult != tc.wantResult {
				t.Fatalf("readiness context = %+v", unavailable)
			}
		})
	}
}

func TestUserSessionTransitionalStatesRespectDeadline(t *testing.T) {
	const uid = uint32(475)
	const username = "pa-namek"
	for _, activeState := range []string{"activating", "deactivating", "reloading"} {
		t.Run(activeState, func(t *testing.T) {
			oldExecutor := defaultExecutor
			defer func() { defaultExecutor = oldExecutor }()
			exec := &mockExecutor{results: map[string]mockResult{
				userSessionShowKey(uid): {output: []byte(fmt.Sprintf("ActiveState=%s\nSubState=waiting\nResult=success\nControlGroup=/user.slice/user-475.slice/user@475.service\n", activeState))},
			}}
			defaultExecutor = exec
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			started := time.Now()
			err := waitForUserSession(ctx, "namek", username, uid, true)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("waitForUserSession error = %v, want deadline", err)
			}
			if elapsed := time.Since(started); elapsed > time.Second {
				t.Fatalf("transitional wait exceeded bound: %v", elapsed)
			}
		})
	}
}

func TestPostRepairDeadlinePreservesPreAndPostState(t *testing.T) {
	oldExecutor := defaultExecutor
	defer func() { defaultExecutor = oldExecutor }()
	const uid = uint32(475)
	const username = "pa-namek"
	exec := &mockExecutor{results: map[string]mockResult{
		userSessionShowKey(uid): {output: []byte("ActiveState=inactive\nSubState=dead\nResult=success\nControlGroup=\n")},
	}}
	exec.onRun = func(name string, args ...string) {
		if name == "systemctl" && len(args) >= 2 && args[0] == "start" {
			exec.results[userSessionShowKey(uid)] = mockResult{output: []byte("ActiveState=activating\nSubState=start\nResult=success\nControlGroup=/user.slice/user-475.slice/user@475.service\n")}
		}
	}
	defaultExecutor = exec
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := ensureUserSession(ctx, "namek", username, uid)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ensureUserSession error = %v, want deadline", err)
	}
	var unavailable *userSessionUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("error type = %T", err)
	}
	if unavailable.PreActionState.ActiveState != "inactive" || unavailable.PostActionState.ActiveState != "activating" ||
		unavailable.RepairAction != "start" || unavailable.RepairResult != "success" {
		t.Fatalf("post-repair readiness context = %+v", unavailable)
	}
}

func TestUserBusProbeHonorsCancellation(t *testing.T) {
	oldExecutor := defaultExecutor
	defer func() { defaultExecutor = oldExecutor }()
	const uid = uint32(475)
	const username = "pa-namek"
	ctx, cancel := context.WithCancel(context.Background())
	exec := &mockExecutor{
		results: map[string]mockResult{
			userSessionShowKey(uid): {output: []byte("ActiveState=active\nSubState=running\nResult=success\nControlGroup=/user.slice/user-475.slice/user@475.service\n")},
		},
		blockUntilContext: map[string]bool{userBusProbeKey(username, uid): true},
	}
	exec.onRun = func(name string, args ...string) {
		if name == "/usr/sbin/runuser" {
			cancel()
		}
	}
	defaultExecutor = exec
	err := waitForUserSession(ctx, "namek", username, uid, false)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitForUserSession error = %v, want cancellation", err)
	}
}

func TestQuiesceAppUserSessionStopsUnitAndRequiresEmptyState(t *testing.T) {
	oldResolver := defaultResolver
	oldExecutor := defaultExecutor
	oldCgroupRoot := userSessionCgroupRoot
	oldProcessRoot := userProcessRoot
	oldOpen := openProcessPIDFD
	oldSignal := signalProcessPIDFD
	oldClose := closeProcessPIDFD
	defer func() {
		defaultResolver = oldResolver
		defaultExecutor = oldExecutor
		userSessionCgroupRoot = oldCgroupRoot
		userProcessRoot = oldProcessRoot
		openProcessPIDFD = oldOpen
		signalProcessPIDFD = oldSignal
		closeProcessPIDFD = oldClose
	}()

	const uid = uint32(475)
	const username = "pa-namek"
	defaultResolver = &mockResolver{users: map[string]*user.User{
		username: {Uid: "475", Gid: "475", Username: username, HomeDir: "/home/" + username},
	}}
	userSessionCgroupRoot = t.TempDir()
	userProcessRoot = t.TempDir()
	eventsPath := filepath.Join(userSessionCgroupRoot, "user.slice/user-475.slice/user@475.service/cgroup.events")
	if err := os.MkdirAll(filepath.Dir(eventsPath), 0o755); err != nil {
		t.Fatalf("create cgroup fixture: %v", err)
	}
	if err := os.WriteFile(eventsPath, []byte("populated 0\n"), 0o644); err != nil {
		t.Fatalf("write cgroup fixture: %v", err)
	}
	exec := &mockExecutor{results: map[string]mockResult{
		userSessionShowKey(uid): {output: []byte("ActiveState=active\nSubState=running\nResult=success\nControlGroup=/user.slice/user-475.slice/user@475.service\n")},
	}}
	exec.onRun = func(name string, args ...string) {
		if name == "systemctl" && len(args) >= 2 && args[0] == "stop" {
			exec.results[userSessionShowKey(uid)] = mockResult{output: []byte("ActiveState=inactive\nSubState=dead\nResult=success\nControlGroup=\n")}
		}
	}
	defaultExecutor = exec
	escapedProc := filepath.Join(userProcessRoot, "4242")
	if err := os.MkdirAll(escapedProc, 0o755); err != nil {
		t.Fatalf("create escaped process fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(escapedProc, "status"), []byte("Name:\tcatatonit\nUid:\t475\t475\t475\t475\n"), 0o644); err != nil {
		t.Fatalf("write escaped process fixture: %v", err)
	}
	openProcessPIDFD = func(pid int, _ int) (int, error) { return pid, nil }
	escapedSignaled := false
	signalProcessPIDFD = func(_ int, signal unix.Signal) error {
		if signal != unix.SIGKILL {
			t.Fatalf("escaped process signal = %v, want SIGKILL", signal)
		}
		escapedSignaled = true
		return os.RemoveAll(escapedProc)
	}
	closeProcessPIDFD = func(int) error { return nil }

	if err := QuiesceAppUserSession(context.Background(), "namek"); err != nil {
		t.Fatalf("QuiesceAppUserSession: %v", err)
	}
	if !hasExecutorCall(exec.calls, "systemctl", "stop", "user@475.service") {
		t.Fatalf("expected PID 1 stop, calls=%v", exec.calls)
	}
	if !escapedSignaled {
		t.Fatal("quiescence did not terminate the UID-owned process outside the user cgroup")
	}
}

func TestProcessStatusHasUIDMatchesAnyCredentialUID(t *testing.T) {
	data := []byte("Name:\tcatatonit\nUid:\t1000\t475\t1000\t1000\n")
	owned, err := processStatusHasUID(data, 475)
	if err != nil {
		t.Fatalf("processStatusHasUID: %v", err)
	}
	if !owned {
		t.Fatal("effective UID match was not recognized")
	}
	owned, err = processStatusHasUID(data, 476)
	if err != nil {
		t.Fatalf("processStatusHasUID non-match: %v", err)
	}
	if owned {
		t.Fatal("unrelated UID was classified as process owner")
	}
}

func TestTerminateUserProcessesKillsEscapedUIDAndProvesAbsence(t *testing.T) {
	oldProcessRoot := userProcessRoot
	oldOpen := openProcessPIDFD
	oldSignal := signalProcessPIDFD
	oldClose := closeProcessPIDFD
	defer func() {
		userProcessRoot = oldProcessRoot
		openProcessPIDFD = oldOpen
		signalProcessPIDFD = oldSignal
		closeProcessPIDFD = oldClose
	}()

	const (
		uid = uint32(475)
		pid = 4242
	)
	userProcessRoot = t.TempDir()
	procDir := filepath.Join(userProcessRoot, strconv.Itoa(pid))
	if err := os.MkdirAll(procDir, 0o755); err != nil {
		t.Fatalf("create process fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(procDir, "status"), []byte("Name:\tcatatonit\nUid:\t475\t475\t475\t475\n"), 0o644); err != nil {
		t.Fatalf("write process fixture: %v", err)
	}

	openProcessPIDFD = func(gotPID int, flags int) (int, error) {
		if gotPID != pid || flags != 0 {
			t.Fatalf("pidfd open = (%d, %d), want (%d, 0)", gotPID, flags, pid)
		}
		return gotPID, nil
	}
	signaled := false
	signalProcessPIDFD = func(fd int, signal unix.Signal) error {
		if fd != pid || signal != unix.SIGKILL {
			t.Fatalf("pidfd signal = (%d, %v), want (%d, SIGKILL)", fd, signal, pid)
		}
		signaled = true
		return os.RemoveAll(procDir)
	}
	closeProcessPIDFD = func(int) error { return nil }

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := terminateUserProcesses(ctx, uid); err != nil {
		t.Fatalf("terminateUserProcesses: %v", err)
	}
	if !signaled {
		t.Fatal("UID-owned process was not signaled")
	}
}

func TestTerminateUserProcessesRechecksOwnershipAfterPidfdOpen(t *testing.T) {
	oldProcessRoot := userProcessRoot
	oldOpen := openProcessPIDFD
	oldSignal := signalProcessPIDFD
	oldClose := closeProcessPIDFD
	defer func() {
		userProcessRoot = oldProcessRoot
		openProcessPIDFD = oldOpen
		signalProcessPIDFD = oldSignal
		closeProcessPIDFD = oldClose
	}()

	const pid = 4343
	userProcessRoot = t.TempDir()
	procDir := filepath.Join(userProcessRoot, strconv.Itoa(pid))
	statusPath := filepath.Join(procDir, "status")
	if err := os.MkdirAll(procDir, 0o755); err != nil {
		t.Fatalf("create process fixture: %v", err)
	}
	if err := os.WriteFile(statusPath, []byte("Name:\told\nUid:\t475\t475\t475\t475\n"), 0o644); err != nil {
		t.Fatalf("write process fixture: %v", err)
	}

	openProcessPIDFD = func(gotPID int, _ int) (int, error) {
		if err := os.WriteFile(statusPath, []byte("Name:\treused\nUid:\t0\t0\t0\t0\n"), 0o644); err != nil {
			t.Fatalf("replace process owner: %v", err)
		}
		return gotPID, nil
	}
	signalProcessPIDFD = func(int, unix.Signal) error {
		t.Fatal("ownership mismatch must not be signaled")
		return nil
	}
	closeProcessPIDFD = func(int) error { return nil }

	if err := terminateUserProcesses(context.Background(), 475); err != nil {
		t.Fatalf("terminateUserProcesses: %v", err)
	}
}

func TestReleaseUserRuntimeStopsOwnerAndRemovesFallbackDirectory(t *testing.T) {
	oldExecutor := defaultExecutor
	oldRuntimeRoot := userRuntimeRoot
	defer func() {
		defaultExecutor = oldExecutor
		userRuntimeRoot = oldRuntimeRoot
	}()

	userRuntimeRoot = t.TempDir()
	runtimeDir := filepath.Join(userRuntimeRoot, "475", "libpod", "tmp")
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		t.Fatalf("create runtime fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, "alive"), []byte("stale"), 0o600); err != nil {
		t.Fatalf("write runtime fixture: %v", err)
	}
	exec := &mockExecutor{results: map[string]mockResult{
		systemUnitShowKey("user-runtime-dir@475.service"): {
			output: []byte("ActiveState=inactive\nSubState=dead\nResult=success\nControlGroup=\n"),
		},
	}}
	defaultExecutor = exec

	if err := releaseUserRuntime(475); err != nil {
		t.Fatalf("releaseUserRuntime: %v", err)
	}
	if !hasExecutorCall(exec.calls, "systemctl", "stop", "user-runtime-dir@475.service") {
		t.Fatalf("runtime-dir owner was not stopped, calls=%v", exec.calls)
	}
	if _, err := os.Stat(filepath.Join(userRuntimeRoot, "475")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("runtime directory still exists: %v", err)
	}
}

func TestReleaseUserRuntimeFailsClosedWhenOwnerRemainsActive(t *testing.T) {
	oldExecutor := defaultExecutor
	oldRuntimeRoot := userRuntimeRoot
	defer func() {
		defaultExecutor = oldExecutor
		userRuntimeRoot = oldRuntimeRoot
	}()

	userRuntimeRoot = t.TempDir()
	runtimeDir := filepath.Join(userRuntimeRoot, "475")
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		t.Fatalf("create runtime fixture: %v", err)
	}
	exec := &mockExecutor{results: map[string]mockResult{
		"systemctl stop user-runtime-dir@475.service": {
			output: []byte("job failed"), err: errors.New("exit status 1"),
		},
		systemUnitShowKey("user-runtime-dir@475.service"): {
			output: []byte("ActiveState=active\nSubState=running\nResult=success\nControlGroup=/user.slice/user-475.slice/user-runtime-dir@475.service\n"),
		},
	}}
	defaultExecutor = exec

	err := releaseUserRuntime(475)
	if err == nil || !strings.Contains(err.Error(), "remains active/running") {
		t.Fatalf("active runtime-dir owner authorized cleanup: %v", err)
	}
	if _, err := os.Stat(runtimeDir); err != nil {
		t.Fatalf("runtime directory was removed without owner quiescence: %v", err)
	}
}

func TestReleaseUserRuntimeRefusesUIDZero(t *testing.T) {
	if err := releaseUserRuntime(0); err == nil {
		t.Fatal("releaseUserRuntime accepted UID 0")
	}
}

func TestDisableLingerRequiresMarkerAbsence(t *testing.T) {
	oldExecutor := defaultExecutor
	oldLingerRoot := userLingerRoot
	defer func() {
		defaultExecutor = oldExecutor
		userLingerRoot = oldLingerRoot
	}()

	userLingerRoot = t.TempDir()
	marker := filepath.Join(userLingerRoot, "pa-namek")
	if err := os.WriteFile(marker, []byte{}, 0o644); err != nil {
		t.Fatalf("create linger marker: %v", err)
	}
	exec := &mockExecutor{results: map[string]mockResult{}}
	defaultExecutor = exec
	if err := disableLinger("pa-namek"); err == nil || !strings.Contains(err.Error(), "still exists") {
		t.Fatalf("persistent linger marker authorized user cleanup: %v", err)
	}

	exec.onRun = func(name string, args ...string) {
		if name == "loginctl" && len(args) == 2 && args[0] == "disable-linger" {
			if err := os.Remove(marker); err != nil {
				t.Fatalf("remove linger marker: %v", err)
			}
		}
	}
	if err := disableLinger("pa-namek"); err != nil {
		t.Fatalf("disableLinger after marker removal: %v", err)
	}
}

func TestQuiesceAppUserSessionMissingUserFailsClosed(t *testing.T) {
	oldResolver := defaultResolver
	oldExecutor := defaultExecutor
	defer func() {
		defaultResolver = oldResolver
		defaultExecutor = oldExecutor
	}()

	defaultResolver = &mockResolver{users: map[string]*user.User{}}
	exec := &mockExecutor{}
	defaultExecutor = exec

	err := QuiesceAppUserSession(context.Background(), "namek")
	if err == nil || !strings.Contains(err.Error(), "cannot prove cgroup empty") {
		t.Fatalf("missing runtime user authorized quiescence: %v", err)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("unexpected systemd calls without a numeric UID: %v", exec.calls)
	}
}

func TestQuiesceAppUserSessionRefusesUIDZeroBeforeSystemdAction(t *testing.T) {
	oldResolver := defaultResolver
	oldExecutor := defaultExecutor
	defer func() {
		defaultResolver = oldResolver
		defaultExecutor = oldExecutor
	}()

	defaultResolver = &mockResolver{users: map[string]*user.User{
		"pa-namek": {Uid: "0", Gid: "0", Username: "root", HomeDir: "/root"},
	}}
	exec := &mockExecutor{}
	defaultExecutor = exec

	err := QuiesceAppUserSession(context.Background(), "namek")
	if err == nil || !strings.Contains(err.Error(), "UID is 0") {
		t.Fatalf("UID 0 app user authorized quiescence: %v", err)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("systemd action occurred before UID 0 rejection: %v", exec.calls)
	}
}

func TestAppUsername_short_name(t *testing.T) {
	got := appUsername("myapp")
	want := "pa-myapp"
	if got != want {
		t.Errorf("appUsername(%q) = %q, want %q", "myapp", got, want)
	}
}

func TestAppUsername_exact_32_chars(t *testing.T) {
	// "pa-" (3 chars) + 29 chars = 32 total, no truncation
	instanceID := strings.Repeat("a", 29)
	got := appUsername(instanceID)
	want := "pa-" + instanceID
	if got != want {
		t.Errorf("appUsername(%q) = %q, want %q", instanceID, got, want)
	}
	if len(got) != 32 {
		t.Errorf("expected length 32, got %d", len(got))
	}
}

func TestAppUsername_truncation(t *testing.T) {
	// "pa-" (3 chars) + 30 chars = 33 total, needs truncation
	instanceID := strings.Repeat("b", 30)
	got := appUsername(instanceID)
	if len(got) != 32 {
		t.Errorf("expected truncated length 32, got %d for %q", len(got), got)
	}
	if !strings.HasPrefix(got, "pa-") {
		t.Errorf("expected prefix 'pa-', got %q", got)
	}
	// Should end with an 8-char hex hash after a dash
	parts := strings.Split(got, "-")
	lastPart := parts[len(parts)-1]
	if len(lastPart) != 8 {
		t.Errorf("expected 8-char hash suffix, got %q (len %d)", lastPart, len(lastPart))
	}
}

func TestAppUsername_deterministic(t *testing.T) {
	id := "my-long-app-instance-name-that-is-very-long"
	a := appUsername(id)
	b := appUsername(id)
	if a != b {
		t.Errorf("appUsername should be deterministic: %q != %q", a, b)
	}
}

func TestAppUsername_different_inputs_different_outputs(t *testing.T) {
	// Two different long inputs that would have the same prefix after truncation
	// should differ due to hash suffix.
	id1 := strings.Repeat("x", 40)
	id2 := strings.Repeat("x", 39) + "y"
	a := appUsername(id1)
	b := appUsername(id2)
	if a == b {
		t.Errorf("different inputs should produce different usernames: both got %q", a)
	}
}

func TestParseSubUIDFile_valid(t *testing.T) {
	content := `piccolo-runtime:100000:65536
pa-myapp:200000:65536
# comment line
pa-other:265536:65536
`
	tmpFile := writeTempFile(t, content)
	entries, err := parseSubUIDFile(tmpFile)
	if err != nil {
		t.Fatalf("parseSubUIDFile: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	tests := []struct {
		username string
		start    uint32
		count    uint32
	}{
		{"piccolo-runtime", 100000, 65536},
		{"pa-myapp", 200000, 65536},
		{"pa-other", 265536, 65536},
	}
	for i, tt := range tests {
		if entries[i].Username != tt.username {
			t.Errorf("entry[%d].Username = %q, want %q", i, entries[i].Username, tt.username)
		}
		if entries[i].Start != tt.start {
			t.Errorf("entry[%d].Start = %d, want %d", i, entries[i].Start, tt.start)
		}
		if entries[i].Count != tt.count {
			t.Errorf("entry[%d].Count = %d, want %d", i, entries[i].Count, tt.count)
		}
	}
}

func TestParseSubUIDFile_empty(t *testing.T) {
	tmpFile := writeTempFile(t, "")
	entries, err := parseSubUIDFile(tmpFile)
	if err != nil {
		t.Fatalf("parseSubUIDFile: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected 0 entries, got %d", len(entries))
	}
}

func TestParseSubUIDFile_nonexistent(t *testing.T) {
	entries, err := parseSubUIDFile("/nonexistent/subuid")
	if err != nil {
		t.Fatalf("expected nil error for nonexistent file, got: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected 0 entries, got %d", len(entries))
	}
}

func TestFindNextSlot_empty(t *testing.T) {
	start, err := findNextSlot(nil, SubUIDBase, SubUIDRangeSize)
	if err != nil {
		t.Fatalf("findNextSlot: %v", err)
	}
	if start != SubUIDBase {
		t.Errorf("expected %d, got %d", SubUIDBase, start)
	}
}

func TestFindNextSlot_after_existing(t *testing.T) {
	entries := []subUIDEntry{
		{Username: "piccolo-runtime", Start: 100000, Count: 65536},
		{Username: "pa-app1", Start: 200000, Count: 65536},
	}
	start, err := findNextSlot(entries, SubUIDBase, SubUIDRangeSize)
	if err != nil {
		t.Fatalf("findNextSlot: %v", err)
	}
	// First slot at 200000 is occupied (200000-265535).
	// Next aligned boundary: ceil(265536 / 65536) * 65536 = 5 * 65536 = 327680
	want := uint32(327680)
	if start != want {
		t.Errorf("expected %d, got %d", want, start)
	}
}

func TestFindNextSlot_gap_filling(t *testing.T) {
	// Second slot (265536) is occupied but first (200000) is free.
	entries := []subUIDEntry{
		{Username: "piccolo-runtime", Start: 100000, Count: 65536},
		{Username: "pa-app2", Start: 265536, Count: 65536},
	}
	start, err := findNextSlot(entries, SubUIDBase, SubUIDRangeSize)
	if err != nil {
		t.Fatalf("findNextSlot: %v", err)
	}
	// First slot at 200000 is free, should pick it.
	want := uint32(200000)
	if start != want {
		t.Errorf("expected %d, got %d", want, start)
	}
}

func TestFindNextSlot_unsorted_entries(t *testing.T) {
	// Entries in reverse order should still work correctly.
	entries := []subUIDEntry{
		{Username: "pa-app2", Start: 265536, Count: 65536},
		{Username: "pa-app1", Start: 200000, Count: 65536},
		{Username: "piccolo-runtime", Start: 100000, Count: 65536},
	}
	start, err := findNextSlot(entries, SubUIDBase, SubUIDRangeSize)
	if err != nil {
		t.Fatalf("findNextSlot: %v", err)
	}
	// 200000-265535 and 265536-331071 both occupied.
	// After aligning past 331071: ceil(331072/65536)*65536 = 6*65536 = 393216.
	want := uint32(393216)
	if start != want {
		t.Errorf("expected %d, got %d", want, start)
	}
}

func TestFindNextSlot_no_overlap_below_base(t *testing.T) {
	// Entries below base should not affect allocation.
	entries := []subUIDEntry{
		{Username: "other-user", Start: 50000, Count: 65536},
	}
	start, err := findNextSlot(entries, SubUIDBase, SubUIDRangeSize)
	if err != nil {
		t.Fatalf("findNextSlot: %v", err)
	}
	if start != SubUIDBase {
		t.Errorf("expected %d, got %d", SubUIDBase, start)
	}
}

func TestDestroyAppUser_nonexistent(t *testing.T) {
	// Destroying a nonexistent user should be a no-op.
	err := DestroyAppUser("nonexistent-app-xyz-12345")
	if err != nil {
		t.Fatalf("DestroyAppUser for nonexistent user should return nil, got: %v", err)
	}
}

func TestResolveAppUser_nonexistent(t *testing.T) {
	_, err := ResolveAppUser("nonexistent-app-xyz-12345")
	if err == nil {
		t.Fatal("ResolveAppUser for nonexistent user should return error")
	}
}

func TestResolveRuntimeCredential_with_mock(t *testing.T) {
	old := defaultResolver
	defer func() { defaultResolver = old }()

	// NOTE: The mock replaces LookupUser, but u.GroupIds() still hits the OS.
	// This works because "test-runtime" doesn't exist in /etc/group, so GroupIds()
	// returns only the primary GID. If full group isolation is needed, add
	// GroupIds to the CredentialResolver interface.
	defaultResolver = &mockResolver{
		users: map[string]*user.User{
			"test-runtime": {
				Uid:      "1001",
				Gid:      "1001",
				Username: "test-runtime",
				HomeDir:  "/home/test-runtime",
			},
		},
	}

	ru, err := ResolveRuntimeCredential("test-runtime")
	if err != nil {
		t.Fatalf("ResolveRuntimeCredential: %v", err)
	}
	if ru.Credential.Uid != 1001 {
		t.Errorf("expected UID 1001, got %d", ru.Credential.Uid)
	}
	if ru.Credential.Gid != 1001 {
		t.Errorf("expected GID 1001, got %d", ru.Credential.Gid)
	}
	if ru.HomeDir != "/home/test-runtime" {
		t.Errorf("expected HomeDir /home/test-runtime, got %s", ru.HomeDir)
	}
}

func TestResolveRuntimeCredential_mock_not_found(t *testing.T) {
	old := defaultResolver
	defer func() { defaultResolver = old }()

	defaultResolver = &mockResolver{users: map[string]*user.User{}}

	_, err := ResolveRuntimeCredential("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent user")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' in error, got: %v", err)
	}
}

func TestDestroyAppUser_UID0_guard_with_mock(t *testing.T) {
	oldResolver := defaultResolver
	oldExecutor := defaultExecutor
	defer func() {
		defaultResolver = oldResolver
		defaultExecutor = oldExecutor
	}()

	defaultResolver = &mockResolver{
		users: map[string]*user.User{
			"pa-evil": {Uid: "0", Gid: "0", Username: "pa-evil", HomeDir: "/root"},
		},
	}
	exec := &mockExecutor{results: map[string]mockResult{}}
	defaultExecutor = exec

	err := destroyAppUserByName("pa-evil")
	if err == nil {
		t.Fatal("expected error when UID is 0")
	}
	if !strings.Contains(err.Error(), "UID is 0") {
		t.Errorf("expected 'UID is 0' in error, got: %v", err)
	}
	// Verify no system commands were executed (no kill/userdel for root).
	if len(exec.calls) != 0 {
		t.Errorf("expected no system commands for UID 0, got: %v", exec.calls)
	}
}

func TestPodmanRuntime_Validate(t *testing.T) {
	tests := []struct {
		name    string
		rt      PodmanRuntime
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid_runtime",
			rt: PodmanRuntime{
				Credential: &syscall.Credential{Uid: 1000, Gid: 1000},
				HomeDir:    "/home/user",
			},
			wantErr: false,
		},
		{
			name:    "nil_credential",
			rt:      PodmanRuntime{HomeDir: "/home/user"},
			wantErr: true,
			errMsg:  "Credential is nil",
		},
		{
			name: "uid_zero",
			rt: PodmanRuntime{
				Credential: &syscall.Credential{Uid: 0, Gid: 0},
				HomeDir:    "/root",
			},
			wantErr: true,
			errMsg:  "UID is 0",
		},
		{
			name: "empty_homedir",
			rt: PodmanRuntime{
				Credential: &syscall.Credential{Uid: 1000, Gid: 1000},
			},
			wantErr: true,
			errMsg:  "HomeDir is empty",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.rt.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.errMsg != "" && err != nil && !strings.Contains(err.Error(), tt.errMsg) {
				t.Errorf("expected error containing %q, got: %v", tt.errMsg, err)
			}
		})
	}
}

func TestProvisionAppUser_useradd_failure_returns_error(t *testing.T) {
	oldResolver := defaultResolver
	oldExecutor := defaultExecutor
	defer func() {
		defaultResolver = oldResolver
		defaultExecutor = oldExecutor
	}()

	// NOTE: hasSubUIDAllocation/allocateSubUIDRange still read the real /etc/subuid.
	// This test works because the mock username won't appear in /etc/subuid.

	// Mock: user does not exist.
	defaultResolver = &mockResolver{
		users:  map[string]*user.User{},
		groups: map[string]*user.Group{},
	}

	// Mock: useradd fails.
	exec := &mockExecutor{
		results: map[string]mockResult{},
		defaultResult: mockResult{
			output: []byte("command failed"),
			err:    fmt.Errorf("exit status 1"),
		},
	}
	defaultExecutor = exec

	_, err := ProvisionAppUser("test-fail-app")
	if err == nil {
		t.Fatal("expected error from ProvisionAppUser when useradd fails")
	}
	if !strings.Contains(err.Error(), "useradd") {
		t.Errorf("expected 'useradd' in error, got: %v", err)
	}
}

func TestProvisionAppUser_usermod_failure_triggers_rollback(t *testing.T) {
	oldResolver := defaultResolver
	oldExecutor := defaultExecutor
	defer func() {
		defaultResolver = oldResolver
		defaultExecutor = oldExecutor
	}()

	// NOTE: hasSubUIDAllocation/allocateSubUIDRange still read the real /etc/subuid.
	// This test works because the mock username won't appear in /etc/subuid.

	username := appUsername("test-rollback-app")

	// Resolver: user doesn't exist initially.
	// The onRun callback makes the user visible after useradd succeeds.
	resolver := &mockResolver{
		users:  map[string]*user.User{},
		groups: map[string]*user.Group{},
	}
	defaultResolver = resolver

	exec := &mockExecutor{
		results: map[string]mockResult{},
		defaultResult: mockResult{
			output: []byte("command failed"),
			err:    fmt.Errorf("exit status 1"),
		},
	}
	// useradd succeeds; usermod (subuid) fails via default.
	exec.results[strings.Join([]string{"useradd", "--system", "--shell", "/usr/sbin/nologin",
		"--create-home", username}, " ")] = mockResult{output: nil, err: nil}
	defaultExecutor = exec

	// When useradd runs, make the user visible in the resolver so that
	// the rollback defer (which checks userExists) can find it.
	exec.onRun = func(name string, args ...string) {
		if name == "useradd" {
			resolver.users[username] = &user.User{
				Uid: "5000", Gid: "5000", Username: username, HomeDir: "/home/" + username,
			}
		}
	}

	_, err := ProvisionAppUser("test-rollback-app")
	if err == nil {
		t.Fatal("expected error from ProvisionAppUser when usermod fails")
	}

	// The rollback defer should attempt userdel since useradd created the user
	// (userExists was false) and a subsequent step failed.
	var foundRollback bool
	for _, call := range exec.calls {
		if len(call) >= 3 && call[0] == "userdel" && call[1] == "--remove" && call[2] == username {
			foundRollback = true
			break
		}
	}
	if !foundRollback {
		t.Errorf("expected rollback userdel for %s, calls were: %v", username, exec.calls)
	}
}

func writeTempFile(t *testing.T, content string) string {
	t.Helper()
	tmpDir := os.TempDir()
	f, err := os.CreateTemp(tmpDir, "subuid-test-*")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		t.Fatalf("write temp file: %v", err)
	}
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })
	return f.Name()
}
