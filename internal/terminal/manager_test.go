package terminal

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"piccolod/internal/resources/pressure"
)

func testShell() string {
	shell := "/bin/bash"
	if _, err := os.Stat(shell); err != nil {
		shell = "/bin/sh"
	}
	return shell
}

func TestManager_TaskPressureRejectsCreateButKeepsDelete(t *testing.T) {
	gate := pressure.NewAdmissionGate()
	m := NewManager(WithAdmissionGate(gate))
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop(context.Background())
	session, err := m.Create(SessionKindHost, "", hostCmdFactory)
	if err != nil {
		t.Fatal(err)
	}
	gate.Fence()
	if _, err := m.Create(SessionKindHost, "", hostCmdFactory); !pressure.IsAdmissionError(err) {
		t.Fatalf("fenced create error = %v", err)
	}
	if !m.Delete(session.ID) {
		t.Fatal("delete was unavailable while admission fenced")
	}
}

func hostCmdFactory() (*exec.Cmd, error) {
	cmd := exec.Command(testShell())
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	return cmd, nil
}

func TestManager_CreateAndGet(t *testing.T) {
	m := NewManager(WithMaxSessions(4))
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop(context.Background())

	sess, err := m.Create(SessionKindHost, "", hostCmdFactory)
	if err != nil {
		t.Fatal(err)
	}
	if sess.ID == "" {
		t.Fatal("expected non-empty session ID")
	}

	got, ok := m.Get(sess.ID)
	if !ok || got != sess {
		t.Fatal("Get should return the same session")
	}
}

func TestManager_List(t *testing.T) {
	m := NewManager()
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop(context.Background())

	_, err := m.Create(SessionKindHost, "", hostCmdFactory)
	if err != nil {
		t.Fatal(err)
	}

	list := m.List(SessionKindHost, "")
	if len(list) != 1 {
		t.Fatalf("expected 1 host session, got %d", len(list))
	}
	if list[0].Kind != SessionKindHost {
		t.Fatal("expected host kind")
	}

	containerList := m.List(SessionKindContainer, "myapp")
	if len(containerList) != 0 {
		t.Fatalf("expected 0 container sessions, got %d", len(containerList))
	}
}

func TestManager_Delete(t *testing.T) {
	m := NewManager()
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop(context.Background())

	sess, err := m.Create(SessionKindHost, "", hostCmdFactory)
	if err != nil {
		t.Fatal(err)
	}
	id := sess.ID

	if !m.Delete(id) {
		t.Fatal("Delete should return true for existing session")
	}
	if m.Delete(id) {
		t.Fatal("Delete should return false for already-deleted session")
	}

	_, ok := m.Get(id)
	if ok {
		t.Fatal("session should be gone after Delete")
	}
}

func TestManager_MaxSessions(t *testing.T) {
	m := NewManager(WithMaxSessions(2))
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop(context.Background())

	_, err := m.Create(SessionKindHost, "", hostCmdFactory)
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Create(SessionKindHost, "", hostCmdFactory)
	if err != nil {
		t.Fatal(err)
	}

	_, err = m.Create(SessionKindHost, "", hostCmdFactory)
	if err == nil {
		t.Fatal("expected error when max sessions reached")
	}
}

func TestManager_CleanupApp(t *testing.T) {
	m := NewManager()
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop(context.Background())

	// Create two "container" sessions for "myapp" using real shells
	_, err := m.Create(SessionKindContainer, "myapp", hostCmdFactory)
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Create(SessionKindContainer, "myapp", hostCmdFactory)
	if err != nil {
		t.Fatal(err)
	}
	// Create one for a different app
	_, err = m.Create(SessionKindContainer, "other", hostCmdFactory)
	if err != nil {
		t.Fatal(err)
	}

	m.CleanupApp("myapp")

	if len(m.List(SessionKindContainer, "myapp")) != 0 {
		t.Fatal("expected 0 sessions for myapp after cleanup")
	}
	if len(m.List(SessionKindContainer, "other")) != 1 {
		t.Fatal("expected 1 session for other after cleanup")
	}
}

func TestManager_AutoRemoveOnShellExit(t *testing.T) {
	m := NewManager()
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop(context.Background())

	// Use a command that exits immediately
	sess, err := m.Create(SessionKindHost, "", func() (*exec.Cmd, error) {
		return exec.Command("true"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	id := sess.ID

	// Wait for the shell to exit
	select {
	case <-sess.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for shell exit")
	}

	// Give the auto-remove goroutine a moment
	time.Sleep(100 * time.Millisecond)

	_, ok := m.Get(id)
	if ok {
		t.Fatal("session should be auto-removed after shell exit")
	}
}

func TestManager_CreateFailureReleasesReservedSlot(t *testing.T) {
	m := NewManager(WithMaxSessions(1))
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop(context.Background())

	if _, err := m.Create(SessionKindHost, "", func() (*exec.Cmd, error) {
		return exec.Command("/definitely/missing/piccolo-shell"), nil
	}); err == nil {
		t.Fatal("expected PTY start failure")
	}
	if _, err := m.Create(SessionKindHost, "", hostCmdFactory); err != nil {
		t.Fatalf("reserved slot survived failed start: %v", err)
	}
}

func TestManagerRechecksAdmissionAfterCommandFactory(t *testing.T) {
	gate := pressure.NewAdmissionGate()
	m := NewManager(WithAdmissionGate(gate))
	t.Cleanup(gate.ResetForTest)

	_, err := m.CreateContext(context.Background(), SessionKindHost, "", func() (*exec.Cmd, error) {
		gate.FenceCritical()
		return exec.Command(testShell()), nil
	})
	if !pressure.IsAdmissionError(err) {
		t.Fatalf("CreateContext error = %v, want task-pressure admission error", err)
	}
	if got := m.Count(); got != 0 {
		t.Fatalf("session count = %d after rejected factory", got)
	}
}

func TestManager_RepeatedCreateDeleteReapsChildren(t *testing.T) {
	m := NewManager(WithMaxSessions(2))
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop(context.Background())
	baselineGoroutines := runtime.NumGoroutine()

	for i := 0; i < 32; i++ {
		sess, err := m.Create(SessionKindHost, "", hostCmdFactory)
		if err != nil {
			t.Fatalf("create session %d: %v", i, err)
		}
		if !m.Delete(sess.ID) {
			t.Fatalf("delete session %d", i)
		}
		// ProcessState is populated only by Wait/Run. A PTY child cancelled by
		// Delete is commonly signalled, so Exited() itself need not be true.
		if sess.cmd.ProcessState == nil {
			t.Fatalf("session %d child was not waited", i)
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baselineGoroutines+2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > baselineGoroutines+2 {
		t.Fatalf("terminal goroutines did not return near baseline: before=%d after=%d", baselineGoroutines, got)
	}
	if got := len(m.List(SessionKindHost, "")); got != 0 {
		t.Fatalf("host sessions after soak = %d, want 0", got)
	}
}
