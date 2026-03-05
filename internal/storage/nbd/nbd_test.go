package nbd

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"piccolod/internal/testutil"
)

type fakeRunner = testutil.FakeRunner

func TestNewServer_DefaultConfig(t *testing.T) {
	run := &fakeRunner{}
	srv := NewServer(run, ServerConfig{})
	if srv.cfg.SocketDir != "/run/piccolo/nbd" {
		t.Errorf("expected default socket dir, got %s", srv.cfg.SocketDir)
	}
}

func TestServer_StartSession_DuplicateRejects(t *testing.T) {
	run := &fakeRunner{
		// nbd-client -c /dev/nbd0 fails (device is free)
		Errs: map[string]error{
			"nbd-client -c /dev/nbd0": fmt.Errorf("not connected"),
		},
	}
	srv := NewServer(run, ServerConfig{SocketDir: t.TempDir()})

	// Manually insert a session to simulate existing.
	srv.mu.Lock()
	srv.sessions["vol-test"] = &session{volumeID: "vol-test", ready: true}
	srv.mu.Unlock()

	_, err := srv.StartSession(context.Background(), "vol-test", "/dev/vg/lv", 1<<30, DefaultHooks())
	if err == nil {
		t.Fatal("expected error for duplicate session")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestServer_SessionInfo_NotFound(t *testing.T) {
	srv := NewServer(&fakeRunner{}, ServerConfig{SocketDir: t.TempDir()})
	_, ok := srv.SessionInfo("nonexistent")
	if ok {
		t.Error("expected SessionInfo to return false for nonexistent volume")
	}
}

func TestServer_StopSession_Idempotent(t *testing.T) {
	srv := NewServer(&fakeRunner{}, ServerConfig{SocketDir: t.TempDir()})
	// Stopping a non-existent session should not error.
	if err := srv.StopSession(context.Background(), "nonexistent"); err != nil {
		t.Errorf("StopSession for nonexistent should not error: %v", err)
	}
}

func TestServer_AllocateNBDDevice_AllBusy(t *testing.T) {
	// All nbd-client -c calls succeed (devices are in use).
	run := &fakeRunner{}
	srv := NewServer(run, ServerConfig{SocketDir: t.TempDir()})

	_, err := srv.allocateNBDDevice(context.Background())
	if err == nil {
		t.Fatal("expected error when all nbd devices are busy")
	}
	if !strings.Contains(err.Error(), "no free nbd devices") {
		t.Errorf("unexpected error: %v", err)
	}

	calls := run.GetCalls()
	if len(calls) != 16 {
		t.Errorf("expected 16 nbd-client -c calls (nbd0..nbd15), got %d", len(calls))
	}
}

func TestServer_AllocateNBDDevice_FindsFree(t *testing.T) {
	// nbd0 and nbd1 are busy, nbd2 is free.
	run := &fakeRunner{
		Errs: map[string]error{
			"nbd-client -c /dev/nbd2": fmt.Errorf("not connected"),
		},
	}
	srv := NewServer(run, ServerConfig{SocketDir: t.TempDir()})

	dev, err := srv.allocateNBDDevice(context.Background())
	if err != nil {
		t.Fatalf("allocateNBDDevice: %v", err)
	}
	if dev != "/dev/nbd2" {
		t.Errorf("expected /dev/nbd2, got %s", dev)
	}
}

func TestServer_SocketPath(t *testing.T) {
	srv := NewServer(&fakeRunner{}, ServerConfig{SocketDir: "/run/piccolo/nbd"})
	path := srv.socketPath("vol-abc")
	if path != "/run/piccolo/nbd/vol-abc.sock" {
		t.Errorf("unexpected socket path: %s", path)
	}
}

// Hooks tests

func TestDefaultHooks_NoOps(t *testing.T) {
	h := DefaultHooks()

	// ColdRecall should pass through.
	proceed, err := h.ColdRecall.OnRead(context.Background(), 0, 4096)
	if !proceed || err != nil {
		t.Errorf("ColdRecall.OnRead: proceed=%v, err=%v", proceed, err)
	}

	// Eviction should return empty.
	ranges, err := h.Eviction.ShouldEvict(context.Background())
	if len(ranges) != 0 || err != nil {
		t.Errorf("Eviction.ShouldEvict: ranges=%v, err=%v", ranges, err)
	}

	// DirtyBitmap should be empty.
	h.DirtyBitmap.MarkDirty(0, 4096)
	if len(h.DirtyBitmap.DirtyRanges()) != 0 {
		t.Error("NoopDirtyBitmap should return no ranges")
	}
	h.DirtyBitmap.Clear() // should not panic

	// Coalescing should succeed.
	if err := h.Coalescing.OnWrite(context.Background(), 0, 4096); err != nil {
		t.Errorf("Coalescing.OnWrite: %v", err)
	}
}

func TestDefaultServerConfig(t *testing.T) {
	cfg := DefaultServerConfig()
	if cfg.SocketDir != "/run/piccolo/nbd" {
		t.Errorf("expected /run/piccolo/nbd, got %s", cfg.SocketDir)
	}
	if cfg.ReadOnly {
		t.Error("expected ReadOnly to be false by default")
	}
}
