package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"piccolod/internal/persistence"
	"piccolod/internal/remote"
)

type stubRemoteRepo struct {
	cfg       persistence.RemoteConfig
	err       error
	saveCalls int
	saved     persistence.RemoteConfig
}

func (s *stubRemoteRepo) CurrentConfig(context.Context) (persistence.RemoteConfig, error) {
	return s.cfg, s.err
}

func (s *stubRemoteRepo) SaveConfig(_ context.Context, cfg persistence.RemoteConfig) error {
	s.saveCalls++
	s.saved = persistence.RemoteConfig{Payload: append([]byte(nil), cfg.Payload...)}
	if s.err != nil {
		return s.err
	}
	s.cfg = s.saved
	return nil
}

func prepareBootstrapMount(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir mount: %v", err)
	}
}

func TestBootstrapRemoteStorage_LoadSplitFiles(t *testing.T) {
	dir := t.TempDir()
	prepareBootstrapMount(t, dir)
	storage := newBootstrapRemoteStorage(nil, dir)

	remoteDir := filepath.Join(dir, "remote")
	_ = os.MkdirAll(remoteDir, 0o700)

	nexus := remote.NexusConfig{Endpoint: "wss://nexus.example.com", Enabled: true}
	certs := remote.CertInventory{Certificates: []remote.Certificate{{ID: "c1", Status: "ok"}}}
	events := remote.EventLog{Events: []remote.Event{{Level: "info", Message: "hi"}}}

	writeJSON(t, filepath.Join(remoteDir, "nexus.json"), nexus)
	writeJSON(t, filepath.Join(remoteDir, "certificates.json"), certs)
	writeJSON(t, filepath.Join(remoteDir, "events.json"), events)

	got, err := storage.Load(context.Background())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Endpoint != nexus.Endpoint {
		t.Fatalf("expected endpoint %s, got %s", nexus.Endpoint, got.Endpoint)
	}
	if !got.Enabled {
		t.Fatalf("expected enabled=true")
	}
	if len(got.Certificates) != 1 || got.Certificates[0].ID != "c1" {
		t.Fatalf("expected cert c1, got %v", got.Certificates)
	}
	if len(got.Events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got.Events))
	}
}

func TestBootstrapRemoteStorage_LoadPartialFiles(t *testing.T) {
	dir := t.TempDir()
	prepareBootstrapMount(t, dir)
	storage := newBootstrapRemoteStorage(nil, dir)

	remoteDir := filepath.Join(dir, "remote")
	_ = os.MkdirAll(remoteDir, 0o700)

	// Only write nexus.json — certs and events should be zero-valued.
	nexus := remote.NexusConfig{Endpoint: "wss://test"}
	writeJSON(t, filepath.Join(remoteDir, "nexus.json"), nexus)

	got, err := storage.Load(context.Background())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Endpoint != "wss://test" {
		t.Fatalf("expected wss://test, got %s", got.Endpoint)
	}
	if len(got.Certificates) != 0 {
		t.Fatalf("expected 0 certs, got %d", len(got.Certificates))
	}
	if len(got.Events) != 0 {
		t.Fatalf("expected 0 events, got %d", len(got.Events))
	}
}

func TestBootstrapRemoteStorage_LoadFallbackRepo(t *testing.T) {
	dir := t.TempDir()
	prepareBootstrapMount(t, dir)
	want := remote.Config{}
	want.Endpoint = "wss://nexus.example.com/connect"
	payload, _ := json.Marshal(want)
	repo := &stubRemoteRepo{cfg: persistence.RemoteConfig{Payload: payload}}
	storage := newBootstrapRemoteStorage(repo, dir)

	got, err := storage.Load(context.Background())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Endpoint != want.Endpoint {
		t.Fatalf("expected %s, got %s", want.Endpoint, got.Endpoint)
	}
	// Split files should be seeded.
	if _, err := os.Stat(filepath.Join(dir, "remote", "nexus.json")); err != nil {
		t.Fatalf("expected nexus.json seeded, stat err=%v", err)
	}
}

func TestBootstrapRemoteStorage_SaveNexusWritesFileAndRepo(t *testing.T) {
	dir := t.TempDir()
	prepareBootstrapMount(t, dir)
	repo := &stubRemoteRepo{}
	storage := newBootstrapRemoteStorage(repo, dir)

	nexus := remote.NexusConfig{Endpoint: "wss://nexus.example.com/connect", DeviceSecret: "s"}
	certs := remote.CertInventory{Certificates: []remote.Certificate{{ID: "c1"}}}

	if err := storage.SaveNexus(context.Background(), nexus, certs); err != nil {
		t.Fatalf("save: %v", err)
	}
	// nexus.json should exist.
	data, err := os.ReadFile(filepath.Join(dir, "remote", "nexus.json"))
	if err != nil {
		t.Fatalf("read nexus.json: %v", err)
	}
	var fromFile remote.NexusConfig
	if err := json.Unmarshal(data, &fromFile); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if fromFile.Endpoint != nexus.Endpoint {
		t.Fatalf("expected %s, got %s", nexus.Endpoint, fromFile.Endpoint)
	}
	// Repo should have nexus+certs blob.
	if repo.saveCalls != 1 {
		t.Fatalf("expected 1 repo save, got %d", repo.saveCalls)
	}
	var repoCfg remote.Config
	if err := json.Unmarshal(repo.saved.Payload, &repoCfg); err != nil {
		t.Fatalf("repo payload invalid: %v", err)
	}
	if repoCfg.Endpoint != nexus.Endpoint {
		t.Fatalf("repo endpoint: expected %s, got %s", nexus.Endpoint, repoCfg.Endpoint)
	}
	if len(repoCfg.Certificates) != 1 {
		t.Fatalf("repo certs: expected 1, got %d", len(repoCfg.Certificates))
	}
}

func TestBootstrapRemoteStorage_SaveCertsWritesFileAndRepo(t *testing.T) {
	dir := t.TempDir()
	prepareBootstrapMount(t, dir)
	repo := &stubRemoteRepo{}
	storage := newBootstrapRemoteStorage(repo, dir)

	nexus := remote.NexusConfig{Endpoint: "wss://test"}
	certs := remote.CertInventory{Certificates: []remote.Certificate{{ID: "c1", Status: "ok"}}}

	if err := storage.SaveCerts(context.Background(), nexus, certs); err != nil {
		t.Fatalf("save: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "remote", "certificates.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var fromFile remote.CertInventory
	if err := json.Unmarshal(data, &fromFile); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(fromFile.Certificates) != 1 {
		t.Fatalf("expected 1 cert, got %d", len(fromFile.Certificates))
	}
	if repo.saveCalls != 1 {
		t.Fatalf("expected 1 repo save, got %d", repo.saveCalls)
	}
}

func TestBootstrapRemoteStorage_SaveEventsNoRepo(t *testing.T) {
	dir := t.TempDir()
	prepareBootstrapMount(t, dir)
	repo := &stubRemoteRepo{}
	storage := newBootstrapRemoteStorage(repo, dir)

	events := remote.EventLog{Events: []remote.Event{{Level: "info", Message: "test"}}}
	if err := storage.SaveEvents(context.Background(), events); err != nil {
		t.Fatalf("save: %v", err)
	}
	// events.json should exist.
	data, err := os.ReadFile(filepath.Join(dir, "remote", "events.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var fromFile remote.EventLog
	if err := json.Unmarshal(data, &fromFile); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(fromFile.Events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(fromFile.Events))
	}
	// Repo should NOT be touched.
	if repo.saveCalls != 0 {
		t.Fatalf("expected 0 repo saves for events, got %d", repo.saveCalls)
	}
}

func TestBootstrapRemoteStorage_SaveEventsNotMounted(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nonexistent")
	storage := newBootstrapRemoteStorage(nil, dir)

	events := remote.EventLog{Events: []remote.Event{{Level: "info"}}}
	// Should silently succeed when not mounted (events are best-effort).
	if err := storage.SaveEvents(context.Background(), events); err != nil {
		t.Fatalf("expected nil error for unmounted event save, got %v", err)
	}
}

func TestBootstrapRemoteStorage_SaveNexusRepoLocked(t *testing.T) {
	dir := t.TempDir()
	prepareBootstrapMount(t, dir)
	repo := &stubRemoteRepo{err: persistence.ErrLocked}
	storage := newBootstrapRemoteStorage(repo, dir)

	nexus := remote.NexusConfig{Endpoint: "wss://test"}
	certs := remote.CertInventory{}
	err := storage.SaveNexus(context.Background(), nexus, certs)
	if err != nil {
		t.Fatalf("expected nil (filesystem write succeeds even when repo locked), got %v", err)
	}
	// nexus.json should be written to filesystem even when repo is locked.
	data, err := os.ReadFile(filepath.Join(dir, "remote", "nexus.json"))
	if err != nil {
		t.Fatalf("expected nexus.json written, got read err: %v", err)
	}
	var fromFile remote.NexusConfig
	if err := json.Unmarshal(data, &fromFile); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if fromFile.Endpoint != nexus.Endpoint {
		t.Fatalf("expected endpoint %s, got %s", nexus.Endpoint, fromFile.Endpoint)
	}
}

func TestBootstrapRemoteStorage_SaveCertsRepoLocked(t *testing.T) {
	dir := t.TempDir()
	prepareBootstrapMount(t, dir)
	repo := &stubRemoteRepo{err: persistence.ErrLocked}
	storage := newBootstrapRemoteStorage(repo, dir)

	nexus := remote.NexusConfig{Endpoint: "wss://test"}
	certs := remote.CertInventory{Certificates: []remote.Certificate{{ID: "c1", Status: "ok"}}}
	err := storage.SaveCerts(context.Background(), nexus, certs)
	if err != nil {
		t.Fatalf("expected nil (filesystem write succeeds even when repo locked), got %v", err)
	}
	// certificates.json should be written to filesystem even when repo is locked.
	data, err := os.ReadFile(filepath.Join(dir, "remote", "certificates.json"))
	if err != nil {
		t.Fatalf("expected certificates.json written, got read err: %v", err)
	}
	var fromFile remote.CertInventory
	if err := json.Unmarshal(data, &fromFile); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(fromFile.Certificates) != 1 || fromFile.Certificates[0].ID != "c1" {
		t.Fatalf("expected cert c1, got %v", fromFile.Certificates)
	}
	// Repo save was attempted (but failed with ErrLocked).
	if repo.saveCalls != 1 {
		t.Fatalf("expected 1 repo save attempt, got %d", repo.saveCalls)
	}
}

func TestBootstrapRemoteStorage_SaveNexusMountNotReady(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nonexistent")
	repo := &stubRemoteRepo{}
	storage := newBootstrapRemoteStorage(repo, dir)

	nexus := remote.NexusConfig{Endpoint: "wss://test"}
	certs := remote.CertInventory{}
	if err := storage.SaveNexus(context.Background(), nexus, certs); !errors.Is(err, remote.ErrLocked) {
		t.Fatalf("expected remote.ErrLocked when mount missing, got %v", err)
	}
	if repo.saveCalls != 0 {
		t.Fatalf("expected repo save not attempted, got %d", repo.saveCalls)
	}
}

func TestBootstrapRemoteStorage_LoadMountNotReady(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nonexistent")
	storage := newBootstrapRemoteStorage(nil, dir)

	if _, err := storage.Load(context.Background()); !errors.Is(err, remote.ErrLocked) {
		t.Fatalf("expected remote.ErrLocked when mount missing, got %v", err)
	}
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
