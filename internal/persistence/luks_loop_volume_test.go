package persistence

import (
	"context"
	"fmt"
	"strings"
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

func TestLUKSLoopVolume_Init_Commands(t *testing.T) {
	tmpDir := t.TempDir()
	loopFile := tmpDir + "/test.luks"
	run := &loopFakeRunner{
		Outputs: map[string]string{
			"losetup": "/dev/loop0\n",
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
		"truncate",       // sparse file
		"losetup --find", // attach loop
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
	if !strings.Contains(calls[0], "268435456") {
		t.Errorf("truncate should specify size 268435456, got %q", calls[0])
	}

	// Verify mapper name.
	if !strings.Contains(calls[3], "piccolo-loop-test") {
		t.Errorf("cryptsetup open should use mapper name, got %q", calls[3])
	}
}

func TestLUKSLoopVolume_Init_LuksFormatError(t *testing.T) {
	tmpDir := t.TempDir()
	loopFile := tmpDir + "/test.luks"
	run := &loopFakeRunner{
		Outputs: map[string]string{
			"losetup": "/dev/loop0\n",
		},
		Errs: map[string]error{
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
	run := &loopFakeRunner{
		Outputs: map[string]string{
			"losetup": "/dev/loop1\n",
		},
	}
	vol := NewLUKSLoopVolumeWithTmpfs(run, tmpDir+"/tmpfs")

	err := vol.Open(context.Background(), loopFile, []byte("test-key"), mountDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	calls := run.GetCalls()
	expected := []string{
		"losetup --find",
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
	run := &loopFakeRunner{
		Outputs: map[string]string{
			"losetup": "/dev/loop0\n",
		},
		Errs: map[string]error{
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
