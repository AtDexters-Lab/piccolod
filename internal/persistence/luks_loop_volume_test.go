package persistence

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"piccolod/internal/testutil"
)

func TestMapperName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"/piccolo-core/control-plane.luks", "piccolo-loop-control-plane"},
		{"/path/to/volume.img", "piccolo-loop-volume"},
		{"/data/my-vol.luks", "piccolo-loop-my-vol"},
	}
	for _, tc := range tests {
		got := mapperName(tc.input)
		if got != tc.want {
			t.Errorf("mapperName(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestMapperNameForLoop(t *testing.T) {
	got := MapperNameForLoop("/piccolo-core/control-plane.luks")
	if got != "piccolo-loop-control-plane" {
		t.Errorf("MapperNameForLoop = %q", got)
	}
}

type loopFakeRunner = testutil.FakeRunner

// makeExitError runs a shell command that exits with the given code and
// returns the resulting *exec.ExitError. Used by tests that need a real
// ExitError to satisfy errors.As checks in production code.
func makeExitError(code int) *exec.ExitError {
	err := exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr
	}
	panic(fmt.Sprintf("expected *exec.ExitError for exit code %d, got %T: %v", code, err, err))
}

// mountFailOnceRunner wraps FakeRunner to fail the first mount call and
// succeed on subsequent ones. Used to test recovery-then-normal-path flows.
type mountFailOnceRunner struct {
	*loopFakeRunner
	mu         sync.Mutex
	mountCalls int
}

func (r *mountFailOnceRunner) Run(ctx context.Context, name string, args ...string) error {
	if name == "mount" {
		r.mu.Lock()
		r.mountCalls++
		n := r.mountCalls
		r.mu.Unlock()
		if n == 1 {
			// Record the call via the inner runner, but return error.
			_ = r.loopFakeRunner.Run(ctx, name, args...)
			return fmt.Errorf("recovery mount failed")
		}
	}
	return r.loopFakeRunner.Run(ctx, name, args...)
}

func (r *mountFailOnceRunner) RunWithOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	return r.loopFakeRunner.RunWithOutput(ctx, name, args...)
}

func (r *mountFailOnceRunner) RunWithStdin(ctx context.Context, stdin []byte, name string, args ...string) error {
	return r.loopFakeRunner.RunWithStdin(ctx, stdin, name, args...)
}

func (r *mountFailOnceRunner) GetCalls() []string {
	return r.loopFakeRunner.GetCalls()
}

// --- Existing tests (updated for idempotent attachLoop) ---

func TestLUKSLoopVolume_Init_Commands(t *testing.T) {
	tmpDir := t.TempDir()
	loopFile := tmpDir + "/test.luks"
	losetupJKey := "losetup -j " + loopFile
	losetupFindKey := "losetup --find --show " + loopFile
	run := &loopFakeRunner{
		Outputs: map[string]string{
			losetupFindKey: "/dev/loop0\n",
		},
		Errs: map[string]error{
			losetupJKey: fmt.Errorf("not found"),
		},
	}
	vol := NewLUKSLoopVolumeWithTmpfs(run, tmpDir+"/tmpfs")

	err := vol.Init(context.Background(), loopFile, 256<<20, []byte("test-key"))
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	calls := run.GetCalls()

	// Verify key commands in order.
	expected := []string{
		"losetup -j",         // stale loop check
		"truncate",           // sparse file
		"losetup -j",         // attachLoop→findLoop
		"losetup --find",     // attachLoop→new attach
		"cryptsetup luksFormat",
		"cryptsetup open",
		"mkfs.ext4",
		"cryptsetup close",
		"losetup -d", // detach loop
	}
	if len(calls) != len(expected) {
		t.Fatalf("expected %d calls, got %d:\n%s", len(expected), len(calls), strings.Join(calls, "\n"))
	}
	for i, prefix := range expected {
		if !strings.Contains(calls[i], prefix) {
			t.Errorf("call[%d] = %q, want prefix %q", i, calls[i], prefix)
		}
	}

	// Verify truncate size.
	if !strings.Contains(calls[1], "268435456") {
		t.Errorf("truncate should specify size 268435456, got %q", calls[1])
	}

	// Verify mapper name.
	if !strings.Contains(calls[5], "piccolo-loop-test") {
		t.Errorf("cryptsetup open should use mapper name, got %q", calls[5])
	}
}

func TestLUKSLoopVolume_Init_LuksFormatError(t *testing.T) {
	tmpDir := t.TempDir()
	loopFile := tmpDir + "/test.luks"
	losetupJKey := "losetup -j " + loopFile
	losetupFindKey := "losetup --find --show " + loopFile
	run := &loopFakeRunner{
		Outputs: map[string]string{
			losetupFindKey: "/dev/loop0\n",
		},
		Errs: map[string]error{
			losetupJKey:    fmt.Errorf("not found"),
			"cryptsetup": fmt.Errorf("format failed"),
		},
	}
	vol := NewLUKSLoopVolumeWithTmpfs(run, tmpDir+"/tmpfs")

	err := vol.Init(context.Background(), loopFile, 256<<20, []byte("key"))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "luksFormat") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestLUKSLoopVolume_Open_Commands(t *testing.T) {
	tmpDir := t.TempDir()
	loopFile := tmpDir + "/test.luks"
	mountDir := tmpDir + "/mnt"
	losetupJKey := "losetup -j " + loopFile
	losetupFindKey := "losetup --find --show " + loopFile
	run := &loopFakeRunner{
		Outputs: map[string]string{
			losetupFindKey: "/dev/loop1\n",
		},
		Errs: map[string]error{
			losetupJKey: fmt.Errorf("not found"),
		},
	}
	vol := NewLUKSLoopVolumeWithTmpfs(run, tmpDir+"/tmpfs")

	err := vol.Open(context.Background(), loopFile, []byte("test-key"), mountDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	calls := run.GetCalls()
	expected := []string{
		"losetup -j",     // attachLoop→findLoop
		"losetup --find", // attachLoop→new attach
		"cryptsetup open",
		"mount -t ext4",
	}
	if len(calls) != len(expected) {
		t.Fatalf("expected %d calls, got %d:\n%s", len(expected), len(calls), strings.Join(calls, "\n"))
	}
	for i, prefix := range expected {
		if !strings.Contains(calls[i], prefix) {
			t.Errorf("call[%d] = %q, want prefix %q", i, calls[i], prefix)
		}
	}
}

func TestLUKSLoopVolume_Open_CryptsetupError_Rollback(t *testing.T) {
	tmpDir := t.TempDir()
	loopFile := tmpDir + "/test.luks"
	losetupJKey := "losetup -j " + loopFile
	losetupFindKey := "losetup --find --show " + loopFile
	run := &loopFakeRunner{
		Outputs: map[string]string{
			losetupFindKey: "/dev/loop0\n",
		},
		Errs: map[string]error{
			losetupJKey:    fmt.Errorf("not found"),
			"cryptsetup": fmt.Errorf("open failed"),
		},
	}
	vol := NewLUKSLoopVolumeWithTmpfs(run, tmpDir+"/tmpfs")

	err := vol.Open(context.Background(), loopFile, []byte("key"), tmpDir+"/mnt")
	if err == nil {
		t.Fatal("expected error")
	}

	// Should have attempted losetup detach on failure.
	calls := run.GetCalls()
	var hasDetach bool
	for _, c := range calls {
		if strings.Contains(c, "losetup -d") {
			hasDetach = true
		}
	}
	if !hasDetach {
		t.Error("expected losetup -d rollback on open failure")
	}
}

func TestLUKSLoopVolume_Close_Commands(t *testing.T) {
	tmpDir := t.TempDir()
	loopFile := tmpDir + "/test.luks"
	mountDir := tmpDir + "/mnt"
	run := &loopFakeRunner{
		Outputs: map[string]string{
			"losetup": "/dev/loop2: [65025]:131104 (" + loopFile + ")\n",
		},
	}
	vol := NewLUKSLoopVolume(run)

	err := vol.Close(context.Background(), loopFile, mountDir)
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	calls := run.GetCalls()
	expected := []string{
		"losetup -j", // find loop
		"umount",
		"cryptsetup close",
		"losetup -d", // detach loop
	}
	if len(calls) != len(expected) {
		t.Fatalf("expected %d calls, got %d:\n%s", len(expected), len(calls), strings.Join(calls, "\n"))
	}
	for i, prefix := range expected {
		if !strings.Contains(calls[i], prefix) {
			t.Errorf("call[%d] = %q, want prefix %q", i, calls[i], prefix)
		}
	}

	// Verify detach uses the found loop device.
	if !strings.Contains(calls[3], "/dev/loop2") {
		t.Errorf("expected losetup -d /dev/loop2, got %q", calls[3])
	}
}

func TestLUKSLoopVolume_Close_NoLoopDevice(t *testing.T) {
	tmpDir := t.TempDir()
	loopFile := tmpDir + "/test.luks"
	mountDir := tmpDir + "/mnt"
	run := &loopFakeRunner{
		Errs: map[string]error{
			"losetup": fmt.Errorf("not found"),
		},
	}
	vol := NewLUKSLoopVolume(run)

	// Should still succeed — just skip losetup -d.
	err := vol.Close(context.Background(), loopFile, mountDir)
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	calls := run.GetCalls()
	var hasDetach bool
	for _, c := range calls {
		if strings.Contains(c, "losetup -d") {
			hasDetach = true
		}
	}
	if hasDetach {
		t.Error("should not attempt losetup -d when loop device not found")
	}
}

// --- Crash-resilience tests ---

func TestLUKSLoopVolume_Open_AlreadyMounted(t *testing.T) {
	run := &loopFakeRunner{}
	vol := &LUKSLoopVolume{
		run:            run,
		tmpfsDir:       t.TempDir() + "/tmpfs",
		mapperExists:   func(string) bool { return true },
		isMountPoint: func(string) (bool, error) { return true, nil },
	}

	err := vol.Open(context.Background(), "/test.luks", []byte("key"), "/mnt")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if len(run.GetCalls()) != 0 {
		t.Errorf("expected no commands, got: %v", run.GetCalls())
	}
}

func TestLUKSLoopVolume_Open_MapperExists_MountSucceeds(t *testing.T) {
	tmpDir := t.TempDir()
	mountDir := tmpDir + "/mnt"
	run := &loopFakeRunner{}
	vol := &LUKSLoopVolume{
		run:            run,
		tmpfsDir:       tmpDir + "/tmpfs",
		mapperExists:   func(string) bool { return true },
		isMountPoint: func(string) (bool, error) { return false, nil },
	}

	err := vol.Open(context.Background(), tmpDir+"/test.luks", []byte("key"), mountDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	calls := run.GetCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d:\n%s", len(calls), strings.Join(calls, "\n"))
	}
	if !strings.Contains(calls[0], "mount -t ext4") {
		t.Errorf("expected mount, got %q", calls[0])
	}
}

func TestLUKSLoopVolume_Open_MapperExists_MountFails_FreshOpen(t *testing.T) {
	tmpDir := t.TempDir()
	loopFile := tmpDir + "/test.luks"
	mountDir := tmpDir + "/mnt"
	losetupJKey := "losetup -j " + loopFile
	losetupFindKey := "losetup --find --show " + loopFile

	inner := &loopFakeRunner{
		Outputs: map[string]string{
			losetupFindKey: "/dev/loop0\n",
		},
		Errs: map[string]error{
			losetupJKey: fmt.Errorf("not found"),
		},
	}
	run := &mountFailOnceRunner{loopFakeRunner: inner}
	vol := &LUKSLoopVolume{
		run:            run,
		tmpfsDir:       tmpDir + "/tmpfs",
		mapperExists:   func(string) bool { return true },
		isMountPoint: func(string) (bool, error) { return false, nil },
	}

	err := vol.Open(context.Background(), loopFile, []byte("key"), mountDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	calls := run.GetCalls()
	// Recovery mount (fails) → cryptsetup close → losetup -j → losetup --find → cryptsetup open → mount (succeeds)
	expected := []string{
		"mount -t ext4",     // recovery mount (failed)
		"cryptsetup close",  // close stale mapper
		"losetup -j",        // attachLoop→findLoop
		"losetup --find",    // attachLoop→new
		"cryptsetup open",   // unlock
		"mount -t ext4",     // normal mount (succeeds)
	}
	if len(calls) != len(expected) {
		t.Fatalf("expected %d calls, got %d:\n%s", len(expected), len(calls), strings.Join(calls, "\n"))
	}
	for i, prefix := range expected {
		if !strings.Contains(calls[i], prefix) {
			t.Errorf("call[%d] = %q, want prefix %q", i, calls[i], prefix)
		}
	}
}

func TestLUKSLoopVolume_Open_CryptsetupExitCode5_Tolerated(t *testing.T) {
	tmpDir := t.TempDir()
	loopFile := tmpDir + "/test.luks"
	mountDir := tmpDir + "/mnt"
	losetupJKey := "losetup -j " + loopFile
	losetupFindKey := "losetup --find --show " + loopFile

	run := &loopFakeRunner{
		Outputs: map[string]string{
			losetupFindKey: "/dev/loop0\n",
		},
		Errs: map[string]error{
			losetupJKey: fmt.Errorf("not found"),
			"cryptsetup": makeExitError(5),
		},
	}
	vol := NewLUKSLoopVolumeWithTmpfs(run, tmpDir+"/tmpfs")

	err := vol.Open(context.Background(), loopFile, []byte("key"), mountDir)
	if err != nil {
		t.Fatalf("Open should tolerate exit code 5: %v", err)
	}

	// mount should still be called after tolerated exit code 5.
	calls := run.GetCalls()
	var hasMount bool
	for _, c := range calls {
		if strings.Contains(c, "mount -t ext4") {
			hasMount = true
		}
	}
	if !hasMount {
		t.Error("expected mount after tolerated exit code 5")
	}
}

func TestLUKSLoopVolume_Open_CryptsetupExitCode1_NotTolerated(t *testing.T) {
	tmpDir := t.TempDir()
	loopFile := tmpDir + "/test.luks"
	mountDir := tmpDir + "/mnt"
	losetupJKey := "losetup -j " + loopFile
	losetupFindKey := "losetup --find --show " + loopFile

	run := &loopFakeRunner{
		Outputs: map[string]string{
			losetupFindKey: "/dev/loop0\n",
		},
		Errs: map[string]error{
			losetupJKey:  fmt.Errorf("not found"),
			"cryptsetup": makeExitError(1),
		},
	}
	vol := NewLUKSLoopVolumeWithTmpfs(run, tmpDir+"/tmpfs")

	err := vol.Open(context.Background(), loopFile, []byte("key"), mountDir)
	if err == nil {
		t.Fatal("expected error for exit code 1")
	}
	if !strings.Contains(err.Error(), "cryptsetup open") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestLUKSLoopVolume_Open_IsMountPointError_FallsThrough(t *testing.T) {
	tmpDir := t.TempDir()
	loopFile := tmpDir + "/test.luks"
	mountDir := tmpDir + "/mnt"

	run := &loopFakeRunner{}
	vol := &LUKSLoopVolume{
		run:            run,
		tmpfsDir:       tmpDir + "/tmpfs",
		mapperExists:   func(string) bool { return true },
		isMountPoint: func(string) (bool, error) { return false, fmt.Errorf("procfs error") },
	}

	// Recovery path should run (mount succeeds via FakeRunner).
	err := vol.Open(context.Background(), loopFile, []byte("key"), mountDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Should have attempted mount (recovery path, not fast-path return).
	calls := run.GetCalls()
	if len(calls) == 0 {
		t.Fatal("expected commands to be called")
	}
	if !strings.Contains(calls[0], "mount -t ext4") {
		t.Errorf("expected recovery mount attempt, got %q", calls[0])
	}
}

func TestLUKSLoopVolume_Close_LazyUmountFallback(t *testing.T) {
	tmpDir := t.TempDir()
	loopFile := tmpDir + "/test.luks"
	mountDir := tmpDir + "/mnt"
	umountKey := "umount " + mountDir
	run := &loopFakeRunner{
		Outputs: map[string]string{
			"losetup": "/dev/loop0: [65025]:131104 (" + loopFile + ")\n",
		},
		Errs: map[string]error{
			umountKey: fmt.Errorf("device busy"),
		},
	}
	vol := NewLUKSLoopVolume(run)

	err := vol.Close(context.Background(), loopFile, mountDir)
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Should have attempted lazy umount, then continued cleanup.
	calls := run.GetCalls()
	var hasLazy bool
	for _, c := range calls {
		if strings.Contains(c, "umount -l") {
			hasLazy = true
		}
	}
	if !hasLazy {
		t.Error("expected lazy umount fallback")
	}

	// All cleanup steps should have run.
	var hasCryptClose, hasDetach bool
	for _, c := range calls {
		if strings.Contains(c, "cryptsetup close") {
			hasCryptClose = true
		}
		if strings.Contains(c, "losetup -d") {
			hasDetach = true
		}
	}
	if !hasCryptClose {
		t.Error("expected cryptsetup close after lazy umount")
	}
	if !hasDetach {
		t.Error("expected losetup -d after cryptsetup close")
	}
}

func TestLUKSLoopVolume_Close_AllFail_CollectsErrors(t *testing.T) {
	tmpDir := t.TempDir()
	loopFile := tmpDir + "/test.luks"
	mountDir := tmpDir + "/mnt"
	umountKey := "umount " + mountDir
	umountLazyKey := "umount -l " + mountDir
	run := &loopFakeRunner{
		Outputs: map[string]string{
			"losetup": "/dev/loop0: [65025]:131104 (" + loopFile + ")\n",
		},
		Errs: map[string]error{
			umountKey:      fmt.Errorf("device busy"),
			umountLazyKey:  fmt.Errorf("lazy also failed"),
			"cryptsetup":  fmt.Errorf("close failed"),
			"losetup -d /dev/loop0": fmt.Errorf("detach failed"),
		},
	}
	vol := NewLUKSLoopVolume(run)

	err := vol.Close(context.Background(), loopFile, mountDir)
	if err == nil {
		t.Fatal("expected error")
	}

	errMsg := err.Error()
	if !strings.Contains(errMsg, "umount") {
		t.Errorf("error should mention umount: %v", err)
	}
	if !strings.Contains(errMsg, "cryptsetup close") {
		t.Errorf("error should mention cryptsetup close: %v", err)
	}
	if !strings.Contains(errMsg, "detach loop") {
		t.Errorf("error should mention detach loop: %v", err)
	}
}

func TestLUKSLoopVolume_Init_StaleCleanup(t *testing.T) {
	tmpDir := t.TempDir()
	loopFile := tmpDir + "/test.luks"
	losetupJKey := "losetup -j " + loopFile
	losetupFindKey := "losetup --find --show " + loopFile
	run := &loopFakeRunner{
		Outputs: map[string]string{
			losetupJKey:    "/dev/loop5: [65025]:131104 (" + loopFile + ")\n",
			losetupFindKey: "/dev/loop0\n",
		},
	}
	vol := &LUKSLoopVolume{
		run:            run,
		tmpfsDir:       tmpDir + "/tmpfs",
		mapperExists:   func(string) bool { return true },
		isMountPoint: func(string) (bool, error) { return false, nil },
	}

	err := vol.Init(context.Background(), loopFile, 256<<20, []byte("key"))
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	calls := run.GetCalls()
	// After stale detach, FakeRunner still returns loop5 for losetup -j since
	// it's keyed by output map — attachLoop reuses it. This is safe in production
	// because luksFormat reformats the device regardless of loop identity.
	expected := []string{
		"cryptsetup close",      // stale mapper cleanup
		"losetup -j",            // stale loop check (finds loop5)
		"losetup -d",            // detach stale loop5
		"truncate",              // sparse file
		"losetup -j",            // attachLoop→findLoop (reuses loop5)
		"cryptsetup luksFormat", // format
		"cryptsetup open",       // open for mkfs
		"mkfs.ext4",             // create filesystem
		"cryptsetup close",      // close after mkfs
		"losetup -d",            // detach loop (defer)
	}
	if len(calls) != len(expected) {
		t.Fatalf("expected %d calls, got %d:\n%s", len(expected), len(calls), strings.Join(calls, "\n"))
	}
	for i, prefix := range expected {
		if !strings.Contains(calls[i], prefix) {
			t.Errorf("call[%d] = %q, want prefix %q", i, calls[i], prefix)
		}
	}
}

func TestLUKSLoopVolume_Init_StaleMapperCloseFailsHard(t *testing.T) {
	tmpDir := t.TempDir()
	loopFile := tmpDir + "/test.luks"
	run := &loopFakeRunner{
		Errs: map[string]error{
			"cryptsetup": fmt.Errorf("close failed"),
		},
	}
	vol := &LUKSLoopVolume{
		run:            run,
		tmpfsDir:       tmpDir + "/tmpfs",
		mapperExists:   func(string) bool { return true },
		isMountPoint: func(string) (bool, error) { return false, nil },
	}

	err := vol.Init(context.Background(), loopFile, 256<<20, []byte("key"))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "close stale mapper") {
		t.Errorf("unexpected error: %v", err)
	}

	// truncate should NOT have been called.
	for _, c := range run.GetCalls() {
		if strings.Contains(c, "truncate") {
			t.Error("truncate should not be called when stale mapper close fails")
		}
	}
}

func TestLUKSLoopVolume_AttachLoop_ReusesExisting(t *testing.T) {
	tmpDir := t.TempDir()
	loopFile := tmpDir + "/test.luks"
	losetupJKey := "losetup -j " + loopFile
	run := &loopFakeRunner{
		Outputs: map[string]string{
			losetupJKey: "/dev/loop3: [65025]:131104 (" + loopFile + ")\n",
		},
	}
	vol := NewLUKSLoopVolumeWithTmpfs(run, tmpDir+"/tmpfs")

	dev, err := vol.attachLoop(context.Background(), loopFile)
	if err != nil {
		t.Fatalf("attachLoop: %v", err)
	}
	if dev != "/dev/loop3" {
		t.Errorf("expected /dev/loop3, got %q", dev)
	}

	// Should NOT have called losetup --find --show.
	for _, c := range run.GetCalls() {
		if strings.Contains(c, "losetup --find") {
			t.Error("should not call losetup --find when existing loop found")
		}
	}
}
