package drbd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeRunner records all Run/RunWithOutput calls.
type fakeRunner struct {
	mu      sync.Mutex
	outputs map[string]string
	errs    map[string]error
	calls   []string
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) error {
	key := name + " " + strings.Join(args, " ")
	f.mu.Lock()
	f.calls = append(f.calls, key)
	err := f.errs[key]
	if err == nil {
		err = f.errs[name]
	}
	f.mu.Unlock()
	return err
}

func (f *fakeRunner) RunWithOutput(_ context.Context, name string, args ...string) ([]byte, error) {
	key := name + " " + strings.Join(args, " ")
	f.mu.Lock()
	f.calls = append(f.calls, key)
	out := f.outputs[key]
	if out == "" {
		out = f.outputs[name]
	}
	err := f.errs[key]
	if err == nil {
		err = f.errs[name]
	}
	f.mu.Unlock()
	return []byte(out), err
}

func (f *fakeRunner) RunWithStdin(_ context.Context, _ []byte, name string, args ...string) error {
	return f.Run(context.Background(), name, args...)
}

func (f *fakeRunner) getCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	copy(out, f.calls)
	return out
}

func TestNewResourceManager_DefaultMetaDir(t *testing.T) {
	mgr := NewResourceManager(&fakeRunner{}, "")
	if mgr.MetaDir() != DefaultMetaDir() {
		t.Errorf("expected default meta dir %s, got %s", DefaultMetaDir(), mgr.MetaDir())
	}
}

func TestNewResourceManager_CustomMetaDir(t *testing.T) {
	mgr := NewResourceManager(&fakeRunner{}, "/custom/meta")
	if mgr.MetaDir() != "/custom/meta" {
		t.Errorf("expected /custom/meta, got %s", mgr.MetaDir())
	}
}

func TestResourceManager_EnsureAvailable(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		run := &fakeRunner{}
		mgr := NewResourceManager(run, t.TempDir())
		if err := mgr.EnsureAvailable(context.Background()); err != nil {
			t.Fatalf("EnsureAvailable: %v", err)
		}
		calls := run.getCalls()
		if len(calls) != 3 {
			t.Fatalf("expected 3 calls, got %d", len(calls))
		}
		if !strings.Contains(calls[0], "modprobe drbd") {
			t.Errorf("expected modprobe drbd, got %q", calls[0])
		}
		if !strings.Contains(calls[1], "drbdadm --version") {
			t.Errorf("expected drbdadm --version, got %q", calls[1])
		}
		if !strings.Contains(calls[2], "drbdsetup --version") {
			t.Errorf("expected drbdsetup --version, got %q", calls[2])
		}
	})

	t.Run("no_kernel_module", func(t *testing.T) {
		run := &fakeRunner{
			errs: map[string]error{
				"modprobe drbd": fmt.Errorf("module not found"),
			},
		}
		mgr := NewResourceManager(run, t.TempDir())
		err := mgr.EnsureAvailable(context.Background())
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "kernel module") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("no_drbdadm", func(t *testing.T) {
		run := &fakeRunner{
			errs: map[string]error{
				"drbdadm --version": fmt.Errorf("not found"),
			},
		}
		mgr := NewResourceManager(run, t.TempDir())
		err := mgr.EnsureAvailable(context.Background())
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "drbdadm not available") {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

func TestResourceManager_DRBDVersion(t *testing.T) {
	run := &fakeRunner{
		outputs: map[string]string{
			"drbdadm --version": "DRBDADM_VERSION=9.27.0\nDRBDSETUP_VERSION=9.27.0\n",
		},
	}
	mgr := NewResourceManager(run, t.TempDir())
	ver, err := mgr.DRBDVersion(context.Background())
	if err != nil {
		t.Fatalf("DRBDVersion: %v", err)
	}
	if ver != "9.27.0" {
		t.Errorf("expected 9.27.0, got %s", ver)
	}
}

func TestResourceManager_ResourceStatus(t *testing.T) {
	run := &fakeRunner{
		outputs: map[string]string{
			"drbdsetup": "vol-abc role:Primary disk:UpToDate\n  peer connection:StandAlone\n",
		},
	}
	mgr := NewResourceManager(run, t.TempDir())
	state, err := mgr.ResourceStatus(context.Background(), "vol-abc")
	if err != nil {
		t.Fatalf("ResourceStatus: %v", err)
	}
	if state.Role != "Primary" {
		t.Errorf("expected Primary, got %s", state.Role)
	}
	if state.DiskState != "UpToDate" {
		t.Errorf("expected UpToDate, got %s", state.DiskState)
	}
	if state.Connection != "StandAlone" {
		t.Errorf("expected StandAlone, got %s", state.Connection)
	}
	if state.DevicePath != "/dev/drbd/by-res/vol-abc" {
		t.Errorf("unexpected device path: %s", state.DevicePath)
	}
}

func TestResourceManager_ResourceStatus_DefaultConnection(t *testing.T) {
	run := &fakeRunner{
		outputs: map[string]string{
			"drbdsetup": "vol-abc role:Primary disk:UpToDate\n",
		},
	}
	mgr := NewResourceManager(run, t.TempDir())
	state, err := mgr.ResourceStatus(context.Background(), "vol-abc")
	if err != nil {
		t.Fatalf("ResourceStatus: %v", err)
	}
	// Connection should default to StandAlone when not in output.
	if state.Connection != "StandAlone" {
		t.Errorf("expected StandAlone default, got %s", state.Connection)
	}
}

func TestResourceManager_ListResources(t *testing.T) {
	run := &fakeRunner{
		outputs: map[string]string{
			"drbdadm sh-resources": "vol-abc\nvol-def\n",
		},
	}
	mgr := NewResourceManager(run, t.TempDir())
	names, err := mgr.ListResources(context.Background())
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	if len(names) != 2 {
		t.Fatalf("expected 2 resources, got %d", len(names))
	}
	if names[0] != "vol-abc" || names[1] != "vol-def" {
		t.Errorf("unexpected names: %v", names)
	}
}

func TestResourceManager_ListResources_Empty(t *testing.T) {
	run := &fakeRunner{
		outputs: map[string]string{
			"drbdadm sh-resources": "",
		},
	}
	mgr := NewResourceManager(run, t.TempDir())
	names, err := mgr.ListResources(context.Background())
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("expected 0 resources, got %d", len(names))
	}
}

func TestDevicePath(t *testing.T) {
	if p := DevicePath("vol-abc"); p != "/dev/drbd/by-res/vol-abc" {
		t.Errorf("unexpected path: %s", p)
	}
}

func TestResourceOps_WriteConfig(t *testing.T) {
	metaDir := t.TempDir()
	confDir := t.TempDir()

	cfg := ResourceConfig{
		Name:          "vol-test",
		BackingDevice: "/dev/nbd0",
		MetaDir:       metaDir,
		ConfDir:       confDir,
		NodeID:        0,
	}
	ops := NewResourceOps(&fakeRunner{}, metaDir, cfg)

	if mp := ops.metaPath(); mp != filepath.Join(metaDir, "vol-test.meta") {
		t.Errorf("unexpected meta path: %s", mp)
	}

	if err := ops.WriteConfig(); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}

	// Verify config file was written.
	confPath := filepath.Join(confDir, "vol-test.res")
	data, err := os.ReadFile(confPath)
	if err != nil {
		t.Fatalf("read config file: %v", err)
	}
	content := string(data)

	// Verify key stanzas are present.
	if !strings.Contains(content, "resource vol-test") {
		t.Error("config missing resource name")
	}
	if !strings.Contains(content, "auto-promote yes") {
		t.Error("config missing auto-promote")
	}
	if !strings.Contains(content, "rs-discard-granularity 65536") {
		t.Error("config missing rs-discard-granularity")
	}
	if !strings.Contains(content, "disk /dev/nbd0") {
		t.Error("config missing backing device")
	}
	if !strings.Contains(content, "node-id 0") {
		t.Error("config missing node-id")
	}
	if !strings.Contains(content, "meta-disk "+filepath.Join(metaDir, "vol-test.meta")) {
		t.Error("config missing meta-disk path")
	}

	// Verify no temp files left behind (atomic write cleanup).
	entries, _ := os.ReadDir(confDir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file not cleaned up: %s", e.Name())
		}
	}
}

func TestResourceOps_DevicePath(t *testing.T) {
	cfg := ResourceConfig{Name: "vol-test"}
	ops := NewResourceOps(&fakeRunner{}, t.TempDir(), cfg)
	if p := ops.DevicePath(); p != "/dev/drbd/by-res/vol-test" {
		t.Errorf("unexpected path: %s", p)
	}
}

func TestResourceOps_Up(t *testing.T) {
	run := &fakeRunner{}
	cfg := ResourceConfig{Name: "vol-test"}
	ops := NewResourceOps(run, t.TempDir(), cfg)

	if err := ops.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}

	calls := run.getCalls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls (up + primary), got %d", len(calls))
	}
	if !strings.Contains(calls[0], "drbdadm up vol-test") {
		t.Errorf("expected drbdadm up, got %q", calls[0])
	}
	if !strings.Contains(calls[1], "drbdadm primary --force vol-test") {
		t.Errorf("expected drbdadm primary --force, got %q", calls[1])
	}
}

func TestResourceOps_Down(t *testing.T) {
	run := &fakeRunner{}
	cfg := ResourceConfig{Name: "vol-test"}
	ops := NewResourceOps(run, t.TempDir(), cfg)

	if err := ops.Down(context.Background()); err != nil {
		t.Fatalf("Down: %v", err)
	}

	calls := run.getCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if !strings.Contains(calls[0], "drbdadm down vol-test") {
		t.Errorf("expected drbdadm down, got %q", calls[0])
	}
}

func TestResourceOps_CreateMetadata(t *testing.T) {
	metaDir := t.TempDir()
	run := &fakeRunner{}
	cfg := ResourceConfig{Name: "vol-test"}
	ops := NewResourceOps(run, metaDir, cfg)

	if err := ops.CreateMetadata(context.Background()); err != nil {
		t.Fatalf("CreateMetadata: %v", err)
	}

	calls := run.getCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if !strings.Contains(calls[0], "drbdadm create-md --force vol-test") {
		t.Errorf("expected drbdadm create-md, got %q", calls[0])
	}
}

func TestResourceOps_RemoveConfig(t *testing.T) {
	metaDir := t.TempDir()
	confDir := filepath.Join(t.TempDir(), "drbd.d")
	_ = os.MkdirAll(confDir, 0o755)

	// Create fake config and metadata files.
	metaFile := filepath.Join(metaDir, "vol-test.meta")
	if err := os.WriteFile(metaFile, []byte("metadata"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := ResourceConfig{Name: "vol-test", MetaDir: metaDir}
	ops := NewResourceOps(&fakeRunner{}, metaDir, cfg)

	// RemoveConfig should remove the metadata file.
	// It will try to remove the config file too (/etc/drbd.d/vol-test.res)
	// which won't exist in test — that's fine (os.IsNotExist is handled).
	if err := ops.RemoveConfig(); err != nil {
		t.Fatalf("RemoveConfig: %v", err)
	}

	if _, err := os.Stat(metaFile); !os.IsNotExist(err) {
		t.Error("expected metadata file to be removed")
	}
}

func TestDefaultMetaDir(t *testing.T) {
	// DefaultMetaDir derives from paths.CoreJoin, which uses the resolved core root.
	// In tests the core root is resolved via env or defaults.
	if d := DefaultMetaDir(); !strings.HasSuffix(d, "/drbd-meta") {
		t.Errorf("expected path ending in /drbd-meta, got %s", d)
	}
}
