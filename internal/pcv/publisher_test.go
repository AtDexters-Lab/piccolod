package pcv

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"piccolod/internal/events"
	"piccolod/internal/state/paths"
)

// stubRunner records commands without executing them.
type stubRunner struct {
	commands [][]string
}

func (r *stubRunner) Run(ctx context.Context, name string, args ...string) error {
	r.commands = append(r.commands, append([]string{name}, args...))
	return nil
}

func (r *stubRunner) RunWithOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	r.commands = append(r.commands, append([]string{name}, args...))
	return nil, nil
}

func (r *stubRunner) RunWithStdin(ctx context.Context, stdin []byte, name string, args ...string) error {
	r.commands = append(r.commands, append([]string{name}, args...))
	return nil
}

func setupTestCoreRoot(t *testing.T) string {
	t.Helper()
	coreRoot := t.TempDir()
	paths.SetCoreRootForTest(t, coreRoot)

	dirs := []string{
		"crypto",
		"volumes/control-plane",
		"mounts/control-plane",
		"recovery",
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(coreRoot, d), 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	// Write a dummy LUKS loop file (simulates the encrypted control plane).
	if err := os.WriteFile(
		filepath.Join(coreRoot, "control-plane.luks"),
		[]byte("LUKS-test-data-placeholder"), 0o600,
	); err != nil {
		t.Fatal(err)
	}

	// Write essential crypto files.
	if err := os.WriteFile(
		filepath.Join(coreRoot, "crypto", "keyset.json"),
		[]byte(`{"sdek":"test","salt":"test","nonce":"test","kdf":{}}`), 0o600,
	); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(
		filepath.Join(coreRoot, "volumes", "control-plane", "piccolo.volume.json"),
		[]byte(`{"passphrase":"test"}`), 0o600,
	); err != nil {
		t.Fatal(err)
	}

	return coreRoot
}

func newTestPublisher(t *testing.T) (*Publisher, string) {
	t.Helper()
	coreRoot := setupTestCoreRoot(t)

	bus := events.NewBus()
	t.Cleanup(func() { bus.Close() })

	pub := NewPublisher(bus, &stubRunner{})
	pub.nodeID = "test-node-1234"
	pub.devFallback = true
	pub.mu.Lock()
	pub.active = true
	pub.mu.Unlock()

	return pub, coreRoot
}

func TestPublisher_PublishNow(t *testing.T) {
	pub, coreRoot := newTestPublisher(t)

	manifest, err := pub.PublishNow(context.Background())
	if err != nil {
		t.Fatalf("PublishNow: %v", err)
	}

	if manifest.Version != manifestVersion {
		t.Errorf("version = %d, want %d", manifest.Version, manifestVersion)
	}
	if manifest.Gen == "" {
		t.Error("generation ID is empty")
	}
	if manifest.SHA256 == "" {
		t.Error("SHA256 is empty")
	}
	if manifest.SizeBytes <= 0 {
		t.Error("size_bytes <= 0")
	}
	if manifest.SourceNodeID != "test-node-1234" {
		t.Errorf("node_id = %q, want %q", manifest.SourceNodeID, "test-node-1234")
	}

	// Verify archive exists.
	if _, err := os.Stat(filepath.Join(coreRoot, "recovery", "current.enc")); err != nil {
		t.Fatalf("archive not found: %v", err)
	}

	// Verify manifest file.
	data, err := os.ReadFile(filepath.Join(coreRoot, "recovery", "current.json"))
	if err != nil {
		t.Fatalf("manifest not found: %v", err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	if m.Gen != manifest.Gen {
		t.Errorf("manifest gen mismatch: file=%q, returned=%q", m.Gen, manifest.Gen)
	}
}

func TestPublisher_GenerationMonotonic(t *testing.T) {
	pub := &Publisher{}

	gen1 := pub.nextGeneration()
	gen2 := pub.nextGeneration()
	gen3 := pub.nextGeneration()

	if gen2 <= gen1 {
		t.Errorf("gen2 (%s) <= gen1 (%s)", gen2, gen1)
	}
	if gen3 <= gen2 {
		t.Errorf("gen3 (%s) <= gen2 (%s)", gen3, gen2)
	}
}

func TestPublisher_GenerationClockSkew(t *testing.T) {
	pub := &Publisher{}

	pub.mu.Lock()
	pub.lastGen = "29991231T235959Z-000010"
	pub.counter = 10
	pub.mu.Unlock()

	gen := pub.nextGeneration()
	if gen <= "29991231T235959Z-000010" {
		t.Errorf("gen should be > lastGen, got %s", gen)
	}
	if !strings.HasPrefix(gen, "29991231T235959Z-") {
		t.Errorf("expected future timestamp prefix, got %s", gen)
	}
}

func TestPublisher_HistoryPruning(t *testing.T) {
	dir := t.TempDir()

	for i := 0; i < historyLimit+3; i++ {
		name := fmt.Sprintf("20260101T%06dZ-000001", i)
		os.WriteFile(filepath.Join(dir, name+".enc"), []byte("archive"), 0o600)
		os.WriteFile(filepath.Join(dir, name+".json"), []byte("{}"), 0o600)
	}

	pub := &Publisher{}
	pub.pruneHistory(dir)

	entries, _ := os.ReadDir(dir)
	encCount := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".enc") {
			encCount++
		}
	}
	if encCount != historyLimit {
		t.Errorf("expected %d archives after pruning, got %d", historyLimit, encCount)
	}
}

func TestPublisher_DirtyLatchClears(t *testing.T) {
	pub, _ := newTestPublisher(t)

	pub.mu.Lock()
	pub.dirty = true
	pub.mu.Unlock()

	if err := pub.doPublish(context.Background()); err != nil {
		t.Fatalf("doPublish: %v", err)
	}

	pub.mu.Lock()
	if pub.dirty {
		t.Error("dirty latch not cleared after successful publish")
	}
	pub.mu.Unlock()
}

func TestPublisher_StopSkipsFlush(t *testing.T) {
	pub, coreRoot := newTestPublisher(t)

	pub.mu.Lock()
	pub.dirty = true
	pub.mu.Unlock()

	if err := pub.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if _, err := os.Stat(filepath.Join(coreRoot, "recovery", "current.enc")); err == nil {
		t.Fatal("Stop should not produce archive — flush-on-shutdown was removed")
	}
}

func TestPublisher_SizeGuard(t *testing.T) {
	pub, coreRoot := newTestPublisher(t)

	// Write a large random (incompressible) loop file to trigger size guard.
	largeData := make([]byte, maxPCVSize+1)
	rand.Read(largeData)
	if err := os.WriteFile(
		filepath.Join(coreRoot, "control-plane.luks"),
		largeData, 0o600,
	); err != nil {
		t.Fatal(err)
	}

	failCh := pub.bus.Subscribe(events.TopicPCVExportFailed, 4)

	err := pub.doPublish(context.Background())
	if err == nil {
		t.Fatal("expected error for oversized archive")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("unexpected error: %v", err)
	}

	select {
	case <-failCh:
		// ok
	case <-time.After(time.Second):
		t.Error("expected PCVExportFailed event")
	}
}

func TestPublisher_NotActiveReturnsError(t *testing.T) {
	bus := events.NewBus()
	defer bus.Close()

	pub := NewPublisher(bus, &stubRunner{})
	_, err := pub.PublishNow(context.Background())
	if err == nil {
		t.Fatal("expected error when not active")
	}
}

func TestPublisher_HistoryArchivesCreated(t *testing.T) {
	pub, coreRoot := newTestPublisher(t)

	// Publish twice.
	if _, err := pub.PublishNow(context.Background()); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	if _, err := pub.PublishNow(context.Background()); err != nil {
		t.Fatalf("second publish: %v", err)
	}

	historyDir := filepath.Join(coreRoot, "recovery", "history")
	entries, err := os.ReadDir(historyDir)
	if err != nil {
		t.Fatalf("read history dir: %v", err)
	}

	encCount := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".enc") {
			encCount++
		}
	}
	if encCount < 2 {
		t.Errorf("expected at least 2 history archives, got %d", encCount)
	}
}
