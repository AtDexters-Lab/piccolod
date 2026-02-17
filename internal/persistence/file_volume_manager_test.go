package persistence

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"piccolod/internal/crypt"
	"piccolod/internal/events"
)

type runnerCall struct {
	name  string
	args  []string
	stdin string
}

type fakeRunner struct {
	calls []runnerCall
}

func (f *fakeRunner) Run(ctx context.Context, name string, args []string, stdin []byte) error {
	call := runnerCall{name: name, args: append([]string(nil), args...), stdin: string(stdin)}
	f.calls = append(f.calls, call)
	return nil
}

type fakeMountLauncher struct {
	calls     []runnerCall
	processes []*fakeMountProcess
}

func (f *fakeMountLauncher) Launch(ctx context.Context, name string, args []string, stdin []byte) (mountProcess, error) {
	call := runnerCall{name: name, args: append([]string(nil), args...), stdin: string(stdin)}
	f.calls = append(f.calls, call)
	proc := &fakeMountProcess{done: make(chan error, 1)}
	f.processes = append(f.processes, proc)
	return proc, nil
}

type fakeMountProcess struct {
	done chan error
}

func (p *fakeMountProcess) Wait() <-chan error {
	return p.done
}

func (p *fakeMountProcess) Signal(os.Signal) error { return nil }

func (p *fakeMountProcess) Kill() error {
	select {
	case p.done <- errors.New("killed"):
	default:
	}
	return nil
}

func (p *fakeMountProcess) Pid() int { return 1234 }

type fakeTimeoutProcess struct {
	waitCh   chan error
	signaled bool
	killed   bool
}

func newFakeTimeoutProcess() *fakeTimeoutProcess {
	return &fakeTimeoutProcess{waitCh: make(chan error, 1)}
}

func (p *fakeTimeoutProcess) Wait() <-chan error {
	return p.waitCh
}

func (p *fakeTimeoutProcess) Signal(os.Signal) error {
	p.signaled = true
	select {
	case p.waitCh <- errors.New("terminated"):
	default:
	}
	return nil
}

func (p *fakeTimeoutProcess) Kill() error {
	p.killed = true
	select {
	case p.waitCh <- errors.New("killed"):
	default:
	}
	return nil
}

func (p *fakeTimeoutProcess) Pid() int {
	return 1234
}

type timeoutMountLauncher struct {
	calls   []runnerCall
	process *fakeTimeoutProcess
}

func (t *timeoutMountLauncher) Launch(ctx context.Context, name string, args []string, stdin []byte) (mountProcess, error) {
	call := runnerCall{name: name, args: append([]string(nil), args...), stdin: string(stdin)}
	t.calls = append(t.calls, call)
	return t.process, nil
}

func newUnlockedCrypto(t *testing.T, dir string) *crypt.Manager {
	mgr, err := crypt.NewManager(dir)
	if err != nil {
		t.Fatalf("new crypto manager: %v", err)
	}
	if err := mgr.Setup("passphrase"); err != nil && !strings.Contains(err.Error(), "already initialized") {
		t.Fatalf("crypto setup: %v", err)
	}
	if err := mgr.Unlock("passphrase"); err != nil {
		t.Fatalf("crypto unlock: %v", err)
	}
	return mgr
}

func TestFileVolumeManagerEnsureVolume(t *testing.T) {
	root := t.TempDir()
	cryptoMgr := newUnlockedCrypto(t, root)
	runner := &fakeRunner{}
	mgr := newFileVolumeManagerWithDeps(root, root, cryptoMgr, runner, "gocryptfs", "fusermount3", nil, nil)

	handle, err := mgr.EnsureVolume(context.Background(), VolumeRequest{ID: "app-init-test", Class: VolumeClassApplication})
	if err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}
	expectedMount := filepath.Join(root, "mounts", "app-init-test")
	if handle.MountDir != expectedMount {
		t.Fatalf("expected mount dir %s, got %s", expectedMount, handle.MountDir)
	}
	if _, err := os.Stat(expectedMount); err != nil {
		t.Fatalf("mount dir missing: %v", err)
	}
	cipherDir := filepath.Join(root, "ciphertext", "app-init-test")
	if _, err := os.Stat(cipherDir); err != nil {
		t.Fatalf("cipher dir missing: %v", err)
	}
	metaPath := filepath.Join(root, "volumes", "app-init-test", volumeMetadataName)
	if _, err := os.Stat(metaPath); err != nil {
		t.Fatalf("metadata missing: %v", err)
	}

	if len(runner.calls) != 1 {
		t.Fatalf("expected one command, got %d", len(runner.calls))
	}
	call := runner.calls[0]
	if call.name != "gocryptfs" || !containsArgs(call.args, []string{"-q", "-init", "-passfile", "/dev/stdin"}) {
		t.Fatalf("unexpected init call: %+v", call)
	}
	if !strings.HasSuffix(call.stdin, "\n") {
		t.Fatalf("expected newline-terminated passphrase, got %q", call.stdin)
	}
	passphrase := strings.TrimSpace(call.stdin)
	if _, err := base64.RawStdEncoding.DecodeString(passphrase); err != nil {
		t.Fatalf("expected base64 passphrase, decode error: %v", err)
	}
	metaBytes, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	var meta volumeMetadata
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	if meta.WrappedKey == "" || meta.Nonce == "" {
		t.Fatalf("metadata missing wrapped key or nonce: %+v", meta)
	}

	// Repeated ensure should not re-run init
	handle2, err := mgr.EnsureVolume(context.Background(), VolumeRequest{ID: "app-init-test", Class: VolumeClassApplication})
	if err != nil {
		t.Fatalf("EnsureVolume second: %v", err)
	}
	if handle2.MountDir != handle.MountDir {
		t.Fatalf("expected same mount dir, got %s vs %s", handle2.MountDir, handle.MountDir)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("expected no additional commands, got %d", len(runner.calls))
	}
}

func TestFileVolumeManagerAttachRoles(t *testing.T) {
	root := t.TempDir()
	cryptoMgr := newUnlockedCrypto(t, root)
	runner := &fakeRunner{}
	launcher := &fakeMountLauncher{}
	mgr := newFileVolumeManagerWithDeps(root, root, cryptoMgr, runner, "gocryptfs", "fusermount3", launcher, func(string, time.Duration) error { return nil })

	h, err := mgr.EnsureVolume(context.Background(), VolumeRequest{ID: "alpha", Class: VolumeClassApplication})
	if err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}
	launcher.calls = launcher.calls[:0]

	if err := mgr.Attach(context.Background(), h, AttachOptions{Role: VolumeRoleLeader}); err != nil {
		t.Fatalf("attach leader: %v", err)
	}
	if len(launcher.calls) != 1 {
		t.Fatalf("expected mount call, got %d", len(launcher.calls))
	}
	if data, err := os.ReadFile(filepath.Join(h.MountDir, ".mode")); err != nil || string(data) != "rw" {
		t.Fatalf("expected mode rw, got %v %q", err, string(data))
	}

	if !containsArgs(launcher.calls[0].args, []string{"-f", "-q", "-passfile", "/dev/stdin"}) {
		t.Fatalf("unexpected leader args: %+v", launcher.calls[0].args)
	}

	// Detach before re-attaching with a different role. Without this,
	// Attach correctly short-circuits (entry.process != nil idempotency).
	if err := mgr.Detach(context.Background(), h); err != nil {
		t.Fatalf("detach before follower: %v", err)
	}
	launcher.calls = launcher.calls[:0]
	if err := mgr.Attach(context.Background(), h, AttachOptions{Role: VolumeRoleFollower}); err != nil {
		t.Fatalf("attach follower: %v", err)
	}
	if len(launcher.calls) != 1 {
		t.Fatalf("expected mount call, got %d", len(launcher.calls))
	}
	call := launcher.calls[0]
	if !containsArgs(call.args, []string{"-ro"}) {
		t.Fatalf("expected -ro in follower args, got %+v", call.args)
	}
	if data, err := os.ReadFile(filepath.Join(h.MountDir, ".mode")); err != nil || string(data) != "ro" {
		t.Fatalf("expected mode ro, got %v %q", err, string(data))
	}
}

func TestFileVolumeManagerAttachIdempotent(t *testing.T) {
	root := t.TempDir()
	cryptoMgr := newUnlockedCrypto(t, root)
	runner := &fakeRunner{}
	launcher := &fakeMountLauncher{}
	mgr := newFileVolumeManagerWithDeps(root, root, cryptoMgr, runner, "gocryptfs", "fusermount3", launcher, func(string, time.Duration) error { return nil })

	h, err := mgr.EnsureVolume(context.Background(), VolumeRequest{ID: "idem", Class: VolumeClassApplication})
	if err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}

	if err := mgr.Attach(context.Background(), h, AttachOptions{Role: VolumeRoleLeader}); err != nil {
		t.Fatalf("first attach: %v", err)
	}
	if len(launcher.calls) != 1 {
		t.Fatalf("expected 1 launch, got %d", len(launcher.calls))
	}

	// Second attach with same role should be idempotent (entry.process != nil).
	launcher.calls = launcher.calls[:0]
	if err := mgr.Attach(context.Background(), h, AttachOptions{Role: VolumeRoleLeader}); err != nil {
		t.Fatalf("second attach (idempotent): %v", err)
	}
	if len(launcher.calls) != 0 {
		t.Fatalf("expected no launch on idempotent attach, got %d", len(launcher.calls))
	}
}

func TestFileVolumeManagerDetach(t *testing.T) {
	root := t.TempDir()
	cryptoMgr := newUnlockedCrypto(t, root)
	runner := &fakeRunner{}
	mgr := newFileVolumeManagerWithDeps(root, root, cryptoMgr, runner, "gocryptfs", "fusermount3", nil, nil)

	h, err := mgr.EnsureVolume(context.Background(), VolumeRequest{ID: "beta", Class: VolumeClassApplication})
	if err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}
	runner.calls = runner.calls[:0]

	if err := mgr.Detach(context.Background(), h); err != nil {
		t.Fatalf("detach: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("expected fusermount call, got %d", len(runner.calls))
	}
	if runner.calls[0].name != "fusermount3" {
		t.Fatalf("expected fusermount3, got %s", runner.calls[0].name)
	}
	if !containsArgs(runner.calls[0].args, []string{"-u", h.MountDir}) {
		t.Fatalf("unexpected fusermount args: %+v", runner.calls[0].args)
	}
}

func TestFileVolumeManagerAttachDetectsCorruptedMetadata(t *testing.T) {
	root := t.TempDir()
	cryptoMgr := newUnlockedCrypto(t, root)
	runner := &fakeRunner{}
	launcher := &fakeMountLauncher{}
	mgr := newFileVolumeManagerWithDeps(root, root, cryptoMgr, runner, "gocryptfs", "fusermount3", launcher, func(string, time.Duration) error { return nil })

	if _, err := mgr.EnsureVolume(context.Background(), VolumeRequest{ID: "corrupt", Class: VolumeClassApplication}); err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}

	metaPath := filepath.Join(root, "volumes", "corrupt", volumeMetadataName)
	if err := os.WriteFile(metaPath, []byte("{"), 0o600); err != nil {
		t.Fatalf("corrupt metadata: %v", err)
	}

	// Simulate manager restart; cached metadata should not mask corruption.
	runner2 := &fakeRunner{}
	launcher2 := &fakeMountLauncher{}
	mgr2 := newFileVolumeManagerWithDeps(root, root, cryptoMgr, runner2, "gocryptfs", "fusermount3", launcher2, func(string, time.Duration) error { return nil })

	_, err := mgr2.EnsureVolume(context.Background(), VolumeRequest{ID: "corrupt", Class: VolumeClassApplication})
	if err == nil {
		t.Fatalf("expected EnsureVolume to fail due to corrupted metadata")
	}
	if !errors.Is(err, ErrVolumeMetadataCorrupted) {
		t.Fatalf("expected ErrVolumeMetadataCorrupted, got %v", err)
	}
	if len(runner2.calls) != 0 {
		t.Fatalf("expected failure before invoking gocryptfs init, got %d calls", len(runner2.calls))
	}
	if len(launcher2.calls) != 0 {
		t.Fatalf("expected failure before launching gocryptfs, got %d calls", len(launcher2.calls))
	}
}

func TestFileVolumeManagerAttachFailsWhenMetadataCorruptedWhileRunning(t *testing.T) {
	root := t.TempDir()
	cryptoMgr := newUnlockedCrypto(t, root)
	runner := &fakeRunner{}
	launcher := &fakeMountLauncher{}
	waiter := func(string, time.Duration) error { return nil }
	mgr := newFileVolumeManagerWithDeps(root, root, cryptoMgr, runner, "gocryptfs", "fusermount3", launcher, waiter)

	handle, err := mgr.EnsureVolume(context.Background(), VolumeRequest{ID: "livecorrupt", Class: VolumeClassApplication})
	if err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}

	metaPath := filepath.Join(root, "volumes", "livecorrupt", volumeMetadataName)
	if err := os.WriteFile(metaPath, []byte("{"), 0o600); err != nil {
		t.Fatalf("corrupt metadata: %v", err)
	}

	err = mgr.Attach(context.Background(), handle, AttachOptions{Role: VolumeRoleLeader})
	if err == nil {
		t.Fatalf("expected Attach to fail due to corrupted metadata")
	}
	if !errors.Is(err, ErrVolumeMetadataCorrupted) {
		t.Fatalf("expected ErrVolumeMetadataCorrupted, got %v", err)
	}
	if len(launcher.calls) != 0 {
		t.Fatalf("expected metadata failure before launching gocryptfs, got %d calls", len(launcher.calls))
	}
}

func TestFileVolumeManagerAttachFailsWithInvalidMetadataValues(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*volumeMetadata)
	}{
		{
			name: "invalid nonce encoding",
			mutate: func(m *volumeMetadata) {
				m.Nonce = "%%%not-base64%%%"
			},
		},
		{
			name: "invalid nonce length",
			mutate: func(m *volumeMetadata) {
				m.Nonce = base64.StdEncoding.EncodeToString([]byte{1, 2, 3})
			},
		},
		{
			name: "invalid wrapped key encoding",
			mutate: func(m *volumeMetadata) {
				m.WrappedKey = "!!!"
			},
		},
		{
			name: "tampered wrapped key",
			mutate: func(m *volumeMetadata) {
				sealed, err := base64.StdEncoding.DecodeString(m.WrappedKey)
				if err != nil || len(sealed) == 0 {
					m.WrappedKey = "invalid"
					return
				}
				sealed[0] ^= 0xFF
				m.WrappedKey = base64.StdEncoding.EncodeToString(sealed)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			cryptoMgr := newUnlockedCrypto(t, root)
			runner := &fakeRunner{}
			launcher := &fakeMountLauncher{}
			waiter := func(string, time.Duration) error { return nil }
			mgr := newFileVolumeManagerWithDeps(root, root, cryptoMgr, runner, "gocryptfs", "fusermount3", launcher, waiter)

			handle, err := mgr.EnsureVolume(context.Background(), VolumeRequest{ID: "victim", Class: VolumeClassApplication})
			if err != nil {
				t.Fatalf("EnsureVolume: %v", err)
			}

			metaPath := filepath.Join(root, "volumes", "victim", volumeMetadataName)
			metaBytes, err := os.ReadFile(metaPath)
			if err != nil {
				t.Fatalf("read metadata: %v", err)
			}

			var meta volumeMetadata
			if err := json.Unmarshal(metaBytes, &meta); err != nil {
				t.Fatalf("unmarshal metadata: %v", err)
			}
			tc.mutate(&meta)
			updated, err := json.MarshalIndent(&meta, "", "  ")
			if err != nil {
				t.Fatalf("marshal metadata: %v", err)
			}
			if err := os.WriteFile(metaPath, updated, 0o600); err != nil {
				t.Fatalf("write metadata: %v", err)
			}

			launcher.calls = nil
			err = mgr.Attach(context.Background(), handle, AttachOptions{Role: VolumeRoleLeader})
			if err == nil {
				t.Fatalf("expected attach to fail due to metadata issue")
			}
			if !errors.Is(err, ErrVolumeMetadataCorrupted) {
				t.Fatalf("expected ErrVolumeMetadataCorrupted, got %v", err)
			}
			if len(launcher.calls) != 0 {
				t.Fatalf("expected metadata failure before launching gocryptfs, got %d calls", len(launcher.calls))
			}
		})
	}
}

func TestFileVolumeManagerAttachHandlesMountTimeout(t *testing.T) {
	root := t.TempDir()
	cryptoMgr := newUnlockedCrypto(t, root)
	runner := &fakeRunner{}
	timeoutProc := newFakeTimeoutProcess()
	launcher := &timeoutMountLauncher{process: timeoutProc}
	waiter := func(string, time.Duration) error {
		return errors.New("mount timed out")
	}
	mgr := newFileVolumeManagerWithDeps(root, root, cryptoMgr, runner, "gocryptfs", "fusermount3", launcher, waiter)

	handle, err := mgr.EnsureVolume(context.Background(), VolumeRequest{ID: "timeout", Class: VolumeClassApplication})
	if err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}

	err = mgr.Attach(context.Background(), handle, AttachOptions{Role: VolumeRoleLeader})
	if err == nil {
		t.Fatalf("expected mount timeout error")
	}
	if !strings.Contains(err.Error(), "wait for mount timeout") && !strings.Contains(err.Error(), "mount timed out") {
		t.Fatalf("expected timeout error, got %v", err)
	}
	if !timeoutProc.signaled {
		t.Fatalf("expected mount process to receive SIGTERM on timeout")
	}
	if timeoutProc.killed {
		t.Fatalf("expected SIGTERM to unblock wait without Kill")
	}
}

func TestFileVolumeManagerRecordsMountedState(t *testing.T) {
	root := t.TempDir()
	cryptoMgr := newUnlockedCrypto(t, root)
	runner := &fakeRunner{}
	launcher := &fakeMountLauncher{}
	waiter := func(string, time.Duration) error { return nil }
	mgr := newFileVolumeManagerWithDeps(root, root, cryptoMgr, runner, "gocryptfs", "fusermount3", launcher, waiter)

	handle, err := mgr.EnsureVolume(context.Background(), VolumeRequest{ID: "journal", Class: VolumeClassApplication})
	if err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}
	if err := mgr.Attach(context.Background(), handle, AttachOptions{Role: VolumeRoleLeader}); err != nil {
		t.Fatalf("Attach leader: %v", err)
	}

	statePath := filepath.Join(root, "volumes", handle.ID, "state.json")
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("expected state journal at %s: %v", statePath, err)
	}

	var state struct {
		Desired     string `json:"desired_state"`
		Observed    string `json:"observed_state"`
		Role        string `json:"role"`
		Generation  int    `json:"generation"`
		NeedsRepair bool   `json:"needs_repair"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("unmarshal journal: %v", err)
	}
	if state.Desired != "mounted" || state.Observed != "mounted" || state.Role != string(VolumeRoleLeader) {
		t.Fatalf("unexpected journal state: %+v", state)
	}
	if state.Generation <= 0 {
		t.Fatalf("expected generation > 0, got %+v", state)
	}
	if state.NeedsRepair {
		t.Fatalf("expected NeedsRepair false after successful mount: %+v", state)
	}
}

func TestFileVolumeManagerPublishesVolumeEvents(t *testing.T) {
	root := t.TempDir()
	cryptoMgr := newUnlockedCrypto(t, root)
	bus := events.NewBus()
	sub := bus.Subscribe(events.TopicVolumeStateChanged, 10)

	runner := &fakeRunner{}
	launcher := &fakeMountLauncher{}
	mgr := newFileVolumeManagerWithDeps(root, root, cryptoMgr, runner, "gocryptfs", "fusermount3", launcher, func(string, time.Duration) error { return nil })
	mgr.bus = bus

	handle, err := mgr.EnsureVolume(context.Background(), VolumeRequest{ID: "eventful", Class: VolumeClassApplication})
	if err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}
	if err := mgr.Attach(context.Background(), handle, AttachOptions{Role: VolumeRoleLeader}); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	var mounted events.VolumeStateChanged
	for mounted.Observed != "mounted" {
		select {
		case evt := <-sub:
			state, ok := evt.Payload.(events.VolumeStateChanged)
			if !ok || state.ID != "eventful" {
				continue
			}
			mounted = state
		case <-ctx.Done():
			t.Fatalf("timeout waiting for mounted event")
		}
	}
	if mounted.Generation <= 0 || mounted.NeedsRepair || mounted.Desired != "mounted" || mounted.Role != string(VolumeRoleLeader) {
		t.Fatalf("unexpected mounted event payload: %+v", mounted)
	}

	if err := mgr.Detach(context.Background(), handle); err != nil {
		t.Fatalf("Detach: %v", err)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	var unmounted events.VolumeStateChanged
	for unmounted.Observed != "unmounted" {
		select {
		case evt := <-sub:
			state, ok := evt.Payload.(events.VolumeStateChanged)
			if !ok || state.ID != "eventful" {
				continue
			}
			unmounted = state
		case <-ctx2.Done():
			t.Fatalf("timeout waiting for unmounted event")
		}
	}
	if unmounted.Desired != "unmounted" || unmounted.NeedsRepair || unmounted.LastError != "" {
		t.Fatalf("unexpected unmounted event payload: %+v", unmounted)
	}
}

func TestFileVolumeManagerReconcilesStaleMountedStateOnEnsure(t *testing.T) {
	root := t.TempDir()
	cryptoMgr := newUnlockedCrypto(t, root)
	runner := &fakeRunner{}
	launcher := &fakeMountLauncher{}
	waiter := func(string, time.Duration) error { return nil }
	mgr := newFileVolumeManagerWithDeps(root, root, cryptoMgr, runner, "gocryptfs", "fusermount3", launcher, waiter)

	handle, err := mgr.EnsureVolume(context.Background(), VolumeRequest{ID: "stale", Class: VolumeClassApplication})
	if err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}
	if err := mgr.Attach(context.Background(), handle, AttachOptions{Role: VolumeRoleLeader}); err != nil {
		t.Fatalf("Attach leader: %v", err)
	}

	// Simulate crash: drop mount markers so the mount is definitely absent.
	if err := os.Remove(filepath.Join(handle.MountDir, ".mode")); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remove mode marker: %v", err)
	}
	if err := os.Remove(filepath.Join(handle.MountDir, ".cipher")); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remove cipher marker: %v", err)
	}

	statePath := filepath.Join(root, "volumes", handle.ID, "state.json")
	initialStateBytes, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read initial state: %v", err)
	}
	var initialState volumeState
	if err := json.Unmarshal(initialStateBytes, &initialState); err != nil {
		t.Fatalf("unmarshal initial state: %v", err)
	}
	if initialState.Observed != "mounted" {
		t.Fatalf("expected initial observed mounted, got %+v", initialState)
	}

	// Restart manager.
	runner2 := &fakeRunner{}
	launcher2 := &fakeMountLauncher{}
	mgr2 := newFileVolumeManagerWithDeps(root, root, cryptoMgr, runner2, "gocryptfs", "fusermount3", launcher2, waiter)

	if err := mgr2.reconcileAllVolumeStates(); err != nil {
		t.Fatalf("reconcileAllVolumeStates: %v", err)
	}

	if len(launcher2.calls) == 0 {
		t.Fatalf("expected auto-reattach during startup")
	}

	if _, err := mgr2.EnsureVolume(context.Background(), VolumeRequest{ID: "stale", Class: VolumeClassApplication}); err != nil {
		t.Fatalf("EnsureVolume after crash: %v", err)
	}

	updatedBytes, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read reconciled state: %v", err)
	}
	var updated volumeState
	if err := json.Unmarshal(updatedBytes, &updated); err != nil {
		t.Fatalf("unmarshal reconciled state: %v", err)
	}
	if updated.Observed != "mounted" {
		t.Fatalf("expected observed=mounted after auto-reattach, got %+v", updated)
	}
	if updated.NeedsRepair {
		t.Fatalf("expected NeedsRepair false after successful reattach %+v", updated)
	}
	if updated.LastError != "" {
		t.Fatalf("expected LastError cleared, got %+v", updated.LastError)
	}
}

func TestFileVolumeManagerReconcilesMissingMountDirOnStartup(t *testing.T) {
	root := t.TempDir()
	cryptoMgr := newUnlockedCrypto(t, root)
	runner := &fakeRunner{}
	launcher := &fakeMountLauncher{}
	waiter := func(string, time.Duration) error { return nil }
	mgr := newFileVolumeManagerWithDeps(root, root, cryptoMgr, runner, "gocryptfs", "fusermount3", launcher, waiter)

	handle, err := mgr.EnsureVolume(context.Background(), VolumeRequest{ID: "missing-mountdir", Class: VolumeClassApplication})
	if err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}
	if err := mgr.Attach(context.Background(), handle, AttachOptions{Role: VolumeRoleLeader}); err != nil {
		t.Fatalf("Attach leader: %v", err)
	}

	if err := os.RemoveAll(handle.MountDir); err != nil {
		t.Fatalf("remove mount dir: %v", err)
	}

	// Restart manager and reconcile. Reattach should recreate the mount directory so marker writes succeed.
	runner2 := &fakeRunner{}
	launcher2 := &fakeMountLauncher{}
	mgr2 := newFileVolumeManagerWithDeps(root, root, cryptoMgr, runner2, "gocryptfs", "fusermount3", launcher2, waiter)

	if err := mgr2.reconcileAllVolumeStates(); err != nil {
		t.Fatalf("reconcileAllVolumeStates: %v", err)
	}
	if len(launcher2.calls) == 0 {
		t.Fatalf("expected auto-reattach during startup")
	}
	if _, err := os.Stat(handle.MountDir); err != nil {
		t.Fatalf("mount dir missing after reconcile: %v", err)
	}
	if _, err := os.Stat(filepath.Join(handle.MountDir, ".mode")); err != nil {
		t.Fatalf("mode marker missing after reconcile: %v", err)
	}
	if _, err := os.Stat(filepath.Join(handle.MountDir, ".cipher")); err != nil {
		t.Fatalf("cipher marker missing after reconcile: %v", err)
	}
}

func TestFileVolumeManagerReconcileSkipsLeaderWithoutAuthority(t *testing.T) {
	root := t.TempDir()
	cryptoMgr := newUnlockedCrypto(t, root)
	launcher := &fakeMountLauncher{}
	mgr := newFileVolumeManagerWithDeps(root, root, cryptoMgr, &fakeRunner{}, "gocryptfs", "fusermount3", launcher, func(string, time.Duration) error { return nil })
	mgr.setRoleChecker(func(string, VolumeRole) bool { return false })

	handle, err := mgr.EnsureVolume(context.Background(), VolumeRequest{ID: "authority", Class: VolumeClassApplication})
	if err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}

	if err := mgr.recordVolumeState(handle.ID, volumeStateMounted, volumeStateMounted, VolumeRoleLeader, nil); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	if err := mgr.reconcileAllVolumeStates(); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := len(launcher.calls); got != 0 {
		t.Fatalf("expected no attach when authority missing, got %d", got)
	}

	mgr.setRoleChecker(func(string, VolumeRole) bool { return true })
	if err := mgr.reconcileAllVolumeStates(); err != nil {
		t.Fatalf("reconcile with authority: %v", err)
	}
	if got := len(launcher.calls); got != 1 {
		t.Fatalf("expected one attach when authority restored, got %d", got)
	}
}

func TestFileVolumeManagerReconcileClearsNeedsRepairOnUnmounted(t *testing.T) {
	root := t.TempDir()
	cryptoMgr := newUnlockedCrypto(t, root)
	mgr := newFileVolumeManagerWithDeps(root, root, cryptoMgr, &fakeRunner{}, "gocryptfs", "fusermount3", nil, func(string, time.Duration) error { return nil })

	handle, err := mgr.EnsureVolume(context.Background(), VolumeRequest{ID: "cleanup", Class: VolumeClassApplication})
	if err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}

	if err := mgr.recordVolumeState(handle.ID, volumeStateUnmounted, volumeStateError, VolumeRoleFollower, errors.New("detach failed")); err != nil {
		t.Fatalf("seed error state: %v", err)
	}

	if err := mgr.reconcileAllVolumeStates(); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	state, err := mgr.readVolumeState(handle.ID)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if state.NeedsRepair {
		t.Fatalf("expected NeedsRepair cleared, got %+v", state)
	}
	if state.Observed != volumeStateUnmounted || state.LastError != "" {
		t.Fatalf("unexpected reconciled state: %+v", state)
	}
}

func TestFileVolumeManagerReconcileHandlesLockedCrypto(t *testing.T) {
	root := t.TempDir()
	cryptoMgr := newUnlockedCrypto(t, root)
	runner := &fakeRunner{}
	waiter := func(string, time.Duration) error { return nil }
	mgr := newFileVolumeManagerWithDeps(root, root, cryptoMgr, runner, "gocryptfs", "fusermount3", nil, waiter)

	handle, err := mgr.EnsureVolume(context.Background(), VolumeRequest{ID: "app-test-volume", Class: VolumeClassApplication})
	if err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}
	if err := mgr.Attach(context.Background(), handle, AttachOptions{Role: VolumeRoleLeader}); err != nil {
		t.Fatalf("Attach leader: %v", err)
	}

	cryptoMgr.Lock()
	lockedMgr := newFileVolumeManagerWithDeps(root, root, cryptoMgr, runner, "gocryptfs", "fusermount3", nil, waiter)
	if err := lockedMgr.reconcileAllVolumeStates(); err != nil {
		t.Fatalf("reconcileAllVolumeStates: %v", err)
	}

	statePath := filepath.Join(root, "volumes", handle.ID, "state.json")
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var state volumeState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if state.Observed != volumeStatePending {
		t.Fatalf("expected observed state pending, got %s", state.Observed)
	}
	if !strings.Contains(state.LastError, "locked") {
		t.Fatalf("expected last error to mention locked, got %q", state.LastError)
	}
}

func TestFileVolumeManagerStateGenerationAndNeedsRepair(t *testing.T) {
	root := t.TempDir()
	cryptoMgr := newUnlockedCrypto(t, root)
	runner := &fakeRunner{}
	launcher := &fakeMountLauncher{}
	waiter := func(string, time.Duration) error { return nil }
	mgr := newFileVolumeManagerWithDeps(root, root, cryptoMgr, runner, "gocryptfs", "fusermount3", launcher, waiter)

	handle, err := mgr.EnsureVolume(context.Background(), VolumeRequest{ID: "gen", Class: VolumeClassApplication})
	if err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}
	if err := mgr.Attach(context.Background(), handle, AttachOptions{Role: VolumeRoleLeader}); err != nil {
		t.Fatalf("Attach leader: %v", err)
	}

	statePath := filepath.Join(root, "volumes", handle.ID, "state.json")
	initialBytes, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var initial volumeState
	if err := json.Unmarshal(initialBytes, &initial); err != nil {
		t.Fatalf("unmarshal initial state: %v", err)
	}
	if initial.Generation == 0 {
		t.Fatalf("expected generation to be > 0, got %+v", initial)
	}
	if initial.NeedsRepair {
		t.Fatalf("expected needsRepair false after successful mount")
	}

	// Simulate crash resulting in missing mount.
	if err := os.Remove(filepath.Join(handle.MountDir, ".mode")); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remove .mode: %v", err)
	}
	if err := os.Remove(filepath.Join(handle.MountDir, ".cipher")); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remove .cipher: %v", err)
	}

	launcherFail := &fakeMountLauncher{}
	waiterFail := func(string, time.Duration) error {
		return errors.New("mount timed out")
	}
	mgr2 := newFileVolumeManagerWithDeps(root, root, cryptoMgr, &fakeRunner{}, "gocryptfs", "fusermount3", launcherFail, waiterFail)
	_ = mgr2.reconcileAllVolumeStates()
	if _, err := mgr2.EnsureVolume(context.Background(), VolumeRequest{ID: "gen", Class: VolumeClassApplication}); err == nil {
		t.Fatalf("expected EnsureVolume to surface auto-reattach failure")
	}

	updatedBytes, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read updated state: %v", err)
	}
	var updated volumeState
	if err := json.Unmarshal(updatedBytes, &updated); err != nil {
		t.Fatalf("unmarshal updated state: %v", err)
	}
	if updated.Generation <= initial.Generation {
		t.Fatalf("expected generation to increment %+v -> %+v", initial, updated)
	}
	if !updated.NeedsRepair {
		t.Fatalf("expected needsRepair true after reconciliation %+v", updated)
	}
}

func containsArgs(args []string, target []string) bool {
	for _, want := range target {
		found := false
		for _, a := range args {
			if a == want {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func TestEnsureVolume_ApplicationClass_UsesDataRoot(t *testing.T) {
	coreRoot := t.TempDir()
	dataRoot := t.TempDir()
	cryptoMgr := newUnlockedCrypto(t, coreRoot)
	runner := &fakeRunner{}
	mgr := newFileVolumeManagerWithDeps(coreRoot, dataRoot, cryptoMgr, runner, "gocryptfs", "fusermount3", nil, nil)

	handle, err := mgr.EnsureVolume(context.Background(), VolumeRequest{ID: "app-myapp", Class: VolumeClassApplication})
	if err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}

	// Mountpoint must be under coreRoot.
	expectedMount := filepath.Join(coreRoot, "mounts", "app-myapp")
	if handle.MountDir != expectedMount {
		t.Fatalf("expected mount dir %s, got %s", expectedMount, handle.MountDir)
	}

	// Ciphertext must be under dataRoot.
	cipherDir := filepath.Join(dataRoot, "ciphertext", "app-myapp")
	if _, err := os.Stat(cipherDir); err != nil {
		t.Fatalf("ciphertext should be under dataRoot: %v", err)
	}
	// Ciphertext must NOT be under coreRoot.
	if _, err := os.Stat(filepath.Join(coreRoot, "ciphertext", "app-myapp")); err == nil {
		t.Fatalf("ciphertext should NOT be under coreRoot")
	}

	// Volume metadata must be under dataRoot.
	metaPath := filepath.Join(dataRoot, "volumes", "app-myapp", volumeMetadataName)
	if _, err := os.Stat(metaPath); err != nil {
		t.Fatalf("metadata should be under dataRoot: %v", err)
	}
	// Volume metadata must NOT be under coreRoot.
	if _, err := os.Stat(filepath.Join(coreRoot, "volumes", "app-myapp", volumeMetadataName)); err == nil {
		t.Fatalf("metadata should NOT be under coreRoot")
	}
}

func TestEnsureVolume_ControlClass_UsesCoreRoot(t *testing.T) {
	t.Setenv("PICCOLO_ALLOW_UNMOUNTED_TESTS", "1") // bypass btrfs for control-class volumes
	coreRoot := t.TempDir()
	dataRoot := t.TempDir()
	cryptoMgr := newUnlockedCrypto(t, coreRoot)
	runner := &fakeRunner{}
	mgr := newFileVolumeManagerWithDeps(coreRoot, dataRoot, cryptoMgr, runner, "gocryptfs", "fusermount3", nil, nil)

	// Use a non-standard control ID to avoid the btrfs subvolume path.
	handle, err := mgr.EnsureVolume(context.Background(), VolumeRequest{ID: "control-test", Class: VolumeClassControl})
	if err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}

	// Mountpoint must be under coreRoot.
	expectedMount := filepath.Join(coreRoot, "mounts", "control-test")
	if handle.MountDir != expectedMount {
		t.Fatalf("expected mount dir %s, got %s", expectedMount, handle.MountDir)
	}

	// Ciphertext and metadata must be under coreRoot.
	if _, err := os.Stat(filepath.Join(coreRoot, "ciphertext", "control-test")); err != nil {
		t.Fatalf("ciphertext should be under coreRoot: %v", err)
	}
	if _, err := os.Stat(filepath.Join(coreRoot, "volumes", "control-test", volumeMetadataName)); err != nil {
		t.Fatalf("metadata should be under coreRoot: %v", err)
	}

	// Nothing under dataRoot for control class.
	if _, err := os.Stat(filepath.Join(dataRoot, "ciphertext", "control-test")); err == nil {
		t.Fatalf("ciphertext should NOT be under dataRoot for control class")
	}

	_ = handle
}

func TestDestroyVolume_ApplicationClass_CleansDataRoot(t *testing.T) {
	coreRoot := t.TempDir()
	dataRoot := t.TempDir()
	cryptoMgr := newUnlockedCrypto(t, coreRoot)
	runner := &fakeRunner{}
	mgr := newFileVolumeManagerWithDeps(coreRoot, dataRoot, cryptoMgr, runner, "gocryptfs", "fusermount3", nil, nil)

	handle, err := mgr.EnsureVolume(context.Background(), VolumeRequest{ID: "app-destroy", Class: VolumeClassApplication})
	if err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataRoot, "ciphertext", "app-destroy")); err != nil {
		t.Fatalf("ciphertext should exist: %v", err)
	}

	if err := mgr.DestroyVolume(context.Background(), handle.ID); err != nil {
		t.Fatalf("DestroyVolume: %v", err)
	}

	// Ciphertext and state under dataRoot should be removed.
	if _, err := os.Stat(filepath.Join(dataRoot, "ciphertext", "app-destroy")); !os.IsNotExist(err) {
		t.Fatalf("ciphertext should be removed from dataRoot")
	}
	if _, err := os.Stat(filepath.Join(dataRoot, "volumes", "app-destroy")); !os.IsNotExist(err) {
		t.Fatalf("volume state should be removed from dataRoot")
	}
	// Mountpoint under coreRoot should also be removed.
	if _, err := os.Stat(filepath.Join(coreRoot, "mounts", "app-destroy")); !os.IsNotExist(err) {
		t.Fatalf("mountpoint should be removed from coreRoot")
	}
}

func TestReconcileAllVolumeStates_ScansBothRoots(t *testing.T) {
	t.Setenv("PICCOLO_ALLOW_UNMOUNTED_TESTS", "1") // bypass btrfs for control-class volumes
	coreRoot := t.TempDir()
	dataRoot := t.TempDir()
	cryptoMgr := newUnlockedCrypto(t, coreRoot)
	runner := &fakeRunner{}
	launcher := &fakeMountLauncher{}
	waiter := func(string, time.Duration) error { return nil }
	mgr := newFileVolumeManagerWithDeps(coreRoot, dataRoot, cryptoMgr, runner, "gocryptfs", "fusermount3", launcher, waiter)

	// Create a control volume under coreRoot.
	if _, err := mgr.EnsureVolume(context.Background(), VolumeRequest{ID: "control-test", Class: VolumeClassControl}); err != nil {
		t.Fatalf("EnsureVolume control: %v", err)
	}
	// Create an app volume under dataRoot.
	if _, err := mgr.EnsureVolume(context.Background(), VolumeRequest{ID: "app-test", Class: VolumeClassApplication}); err != nil {
		t.Fatalf("EnsureVolume app: %v", err)
	}

	// New manager should discover both during reconciliation.
	runner2 := &fakeRunner{}
	launcher2 := &fakeMountLauncher{}
	mgr2 := newFileVolumeManagerWithDeps(coreRoot, dataRoot, cryptoMgr, runner2, "gocryptfs", "fusermount3", launcher2, waiter)
	if err := mgr2.reconcileAllVolumeStates(); err != nil {
		t.Fatalf("reconcileAllVolumeStates: %v", err)
	}

	// Both entries should be in memory.
	mgr2.mu.RLock()
	_, hasControl := mgr2.volumes["control-test"]
	_, hasApp := mgr2.volumes["app-test"]
	mgr2.mu.RUnlock()

	if !hasControl {
		t.Fatalf("expected control-test to be discovered during reconciliation")
	}
	if !hasApp {
		t.Fatalf("expected app-test to be discovered during reconciliation")
	}
}

func TestVolumeStateClassPersisted(t *testing.T) {
	coreRoot := t.TempDir()
	dataRoot := t.TempDir()
	cryptoMgr := newUnlockedCrypto(t, coreRoot)
	runner := &fakeRunner{}
	launcher := &fakeMountLauncher{}
	waiter := func(string, time.Duration) error { return nil }
	mgr := newFileVolumeManagerWithDeps(coreRoot, dataRoot, cryptoMgr, runner, "gocryptfs", "fusermount3", launcher, waiter)

	handle, err := mgr.EnsureVolume(context.Background(), VolumeRequest{ID: "app-class", Class: VolumeClassApplication})
	if err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}
	if err := mgr.Attach(context.Background(), handle, AttachOptions{Role: VolumeRoleLeader}); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	// Read state.json and verify class is persisted.
	statePath := filepath.Join(dataRoot, "volumes", "app-class", "state.json")
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var state volumeState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if state.Class != string(VolumeClassApplication) {
		t.Fatalf("expected class %q, got %q", VolumeClassApplication, state.Class)
	}
}

func TestFileVolumeManagerIntegration(t *testing.T) {
	if os.Getenv("PICCOLO_TEST_GOCRYPTFS") == "" {
		t.Skip("set PICCOLO_TEST_GOCRYPTFS=1 to run gocryptfs integration test")
	}
	if _, err := exec.LookPath("gocryptfs"); err != nil {
		t.Skip("gocryptfs binary not found")
	}
	fusermount := "fusermount3"
	if _, err := exec.LookPath(fusermount); err != nil {
		if _, err := exec.LookPath("fusermount"); err == nil {
			fusermount = "fusermount"
		} else {
			t.Skip("fusermount binary not found")
		}
	}
	if f, err := os.OpenFile("/dev/fuse", os.O_RDWR, 0); err != nil {
		t.Skipf("fuse device unavailable: %v", err)
	} else {
		_ = f.Close()
	}

	root := t.TempDir()
	cryptoMgr := newUnlockedCrypto(t, root)
	mgr := newFileVolumeManagerWithDeps(root, root, cryptoMgr, execRunner{}, "gocryptfs", fusermount, nil, nil)

	h, err := mgr.EnsureVolume(context.Background(), VolumeRequest{ID: "integration", Class: VolumeClassApplication})
	if err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	mounted := false
	t.Cleanup(func() {
		if mounted {
			_ = mgr.Detach(context.Background(), h)
		}
	})

	if err := mgr.Attach(ctx, h, AttachOptions{Role: VolumeRoleLeader}); err != nil {
		t.Fatalf("Attach leader: %v", err)
	}
	mounted = true

	message := []byte("hello from gocryptfs integration test")
	if err := os.WriteFile(filepath.Join(h.MountDir, "test.txt"), message, 0o600); err != nil {
		t.Fatalf("write plaintext: %v", err)
	}

	// Ensure the ciphertext directory does not contain the plaintext string.
	cipherData, err := os.ReadFile(filepath.Join(root, "ciphertext", "integration", "gocryptfs.conf"))
	if err != nil {
		t.Fatalf("read ciphertext metadata: %v", err)
	}
	if strings.Contains(string(cipherData), string(message)) {
		t.Fatalf("ciphertext unexpectedly contains plaintext")
	}

	if err := mgr.Detach(ctx, h); err != nil {
		t.Fatalf("Detach: %v", err)
	}
}

func TestEnsureVolume_IdempotentOnReentry(t *testing.T) {
	t.Setenv("PICCOLO_ALLOW_UNMOUNTED_TESTS", "1")
	root := t.TempDir()
	cryptoMgr := newUnlockedCrypto(t, root)
	runner := &fakeRunner{}
	mgr := newFileVolumeManagerWithDeps(root, root, cryptoMgr, runner, "gocryptfs", "fusermount3", nil, nil)

	// Pre-register entry via getOrCreateEntry (simulates reconcileAllVolumeStates discovery).
	_ = mgr.getOrCreateEntry("app-reentry")

	// EnsureVolume should still create cipher dir and mount dir despite the entry already existing.
	handle, err := mgr.EnsureVolume(context.Background(), VolumeRequest{ID: "app-reentry", Class: VolumeClassApplication})
	if err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}

	cipherDir := filepath.Join(root, "ciphertext", "app-reentry")
	if _, err := os.Stat(cipherDir); err != nil {
		t.Fatalf("expected cipher dir to be created despite pre-registration: %v", err)
	}
	if _, err := os.Stat(handle.MountDir); err != nil {
		t.Fatalf("expected mount dir to be created despite pre-registration: %v", err)
	}
}

func TestEnsureVolume_CipherDirRestoredAfterDeletion(t *testing.T) {
	t.Setenv("PICCOLO_ALLOW_UNMOUNTED_TESTS", "1")
	root := t.TempDir()
	cryptoMgr := newUnlockedCrypto(t, root)
	runner := &fakeRunner{}
	mgr := newFileVolumeManagerWithDeps(root, root, cryptoMgr, runner, "gocryptfs", "fusermount3", nil, nil)

	handle, err := mgr.EnsureVolume(context.Background(), VolumeRequest{ID: "app-restore", Class: VolumeClassApplication})
	if err != nil {
		t.Fatalf("EnsureVolume first: %v", err)
	}

	cipherDir := filepath.Join(root, "ciphertext", "app-restore")
	if _, err := os.Stat(cipherDir); err != nil {
		t.Fatalf("cipher dir should exist after first EnsureVolume: %v", err)
	}

	// Simulate cipher dir deletion (e.g. accidental cleanup).
	if err := os.RemoveAll(cipherDir); err != nil {
		t.Fatalf("remove cipher dir: %v", err)
	}

	// Second EnsureVolume should recreate the cipher dir structure.
	// Note: gocryptfs artifacts (conf/diriv) are not regenerated — full
	// functional recovery requires a separate re-init or restore flow.
	handle2, err := mgr.EnsureVolume(context.Background(), VolumeRequest{ID: "app-restore", Class: VolumeClassApplication})
	if err != nil {
		t.Fatalf("EnsureVolume second: %v", err)
	}
	if handle2.MountDir != handle.MountDir {
		t.Fatalf("expected same mount dir, got %s vs %s", handle2.MountDir, handle.MountDir)
	}
	if _, err := os.Stat(cipherDir); err != nil {
		t.Fatalf("expected cipher dir recreated after deletion: %v", err)
	}

	// Metadata should still be valid after cipher-dir recreation.
	metaPath := filepath.Join(root, "volumes", "app-restore", "piccolo.volume.json")
	if _, err := os.Stat(metaPath); err != nil {
		t.Fatalf("expected volume metadata to exist after recreation: %v", err)
	}
}

func TestEnsureVolume_ControlPlane_CipherDirCreatedDespitePreRegistration(t *testing.T) {
	t.Setenv("PICCOLO_ALLOW_UNMOUNTED_TESTS", "1")
	root := t.TempDir()
	cryptoMgr := newUnlockedCrypto(t, root)
	runner := &fakeRunner{}
	mgr := newFileVolumeManagerWithDeps(root, root, cryptoMgr, runner, "gocryptfs", "fusermount3", nil, nil)

	// Reproduce the exact boot-time bug: manually create volumes/control-plane/
	// (simulating what the old constructor did), then reconcile to pre-register.
	metaDir := filepath.Join(root, "volumes", "control-plane")
	if err := os.MkdirAll(metaDir, 0o700); err != nil {
		t.Fatalf("mkdir metaDir: %v", err)
	}
	if err := mgr.reconcileAllVolumeStates(); err != nil {
		t.Fatalf("reconcileAllVolumeStates: %v", err)
	}

	// Verify entry is pre-registered.
	mgr.mu.RLock()
	_, preRegistered := mgr.volumes["control-plane"]
	mgr.mu.RUnlock()
	if !preRegistered {
		t.Fatalf("expected control-plane to be pre-registered by reconcile")
	}

	// EnsureVolume must create the cipher dir despite early-return path.
	_, err := mgr.EnsureVolume(context.Background(), VolumeRequest{ID: "control-plane", Class: VolumeClassControl})
	if err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}

	cipherDir := filepath.Join(root, "ciphertext", "control-plane")
	if _, err := os.Stat(cipherDir); err != nil {
		t.Fatalf("expected cipher dir to be created despite pre-registration: %v", err)
	}
}
