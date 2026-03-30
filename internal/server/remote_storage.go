package server

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"

	"piccolod/internal/fsutil"
	"piccolod/internal/persistence"
	"piccolod/internal/remote"
	"piccolod/internal/state/paths"
)

type bootstrapRemoteStorage struct {
	repo persistence.RemoteRepo
	dir  string // directory for individual files (e.g., baseDir/remote/)
	root string // base dir for isMounted check
}

func newBootstrapRemoteStorage(repo persistence.RemoteRepo, baseDir string) remote.Storage {
	if baseDir == "" {
		baseDir = paths.CoreRoot()
	}
	return &bootstrapRemoteStorage{
		repo: repo,
		dir:  filepath.Join(baseDir, "remote"),
		root: baseDir,
	}
}

func (s *bootstrapRemoteStorage) Load(ctx context.Context) (remote.Config, error) {
	if s == nil {
		return remote.Config{}, errors.New("remote storage: unavailable")
	}

	// Pre-unlock: can only access repo.
	if !s.isMounted() {
		return s.loadFromRepo(ctx)
	}

	// Try loading from split files on filesystem.
	if cfg, found := remote.LoadSplitFiles(s.dir); found {
		return cfg, nil
	}

	// No split files found — fall back to repo and seed bootstrap files.
	// When mounted but repo is nil (first boot, no encrypted store yet), return empty config.
	if s.repo == nil {
		return remote.Config{}, nil
	}
	cfg, err := s.loadFromRepo(ctx)
	if err != nil {
		return cfg, err
	}
	if cfg.Endpoint != "" || len(cfg.Certificates) > 0 {
		s.seedBootstrapFiles(&cfg)
	}
	return cfg, nil
}

func (s *bootstrapRemoteStorage) loadFromRepo(ctx context.Context) (remote.Config, error) {
	if s.repo == nil {
		return remote.Config{}, remote.ErrLocked
	}
	repoCfg, err := s.repo.CurrentConfig(ctx)
	if err != nil {
		if errors.Is(err, persistence.ErrLocked) {
			return remote.Config{}, remote.ErrLocked
		}
		if errors.Is(err, persistence.ErrNotFound) {
			return remote.Config{}, nil
		}
		return remote.Config{}, err
	}
	if len(repoCfg.Payload) == 0 {
		return remote.Config{}, nil
	}
	var cfg remote.Config
	if err := json.Unmarshal(repoCfg.Payload, &cfg); err != nil {
		return remote.Config{}, err
	}
	return cfg, nil
}

func (s *bootstrapRemoteStorage) seedBootstrapFiles(cfg *remote.Config) {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		log.Printf("WARN: failed to create bootstrap remote config dir: %v", err)
		return
	}
	if p, err := json.MarshalIndent(&cfg.NexusConfig, "", "  "); err == nil {
		if err := fsutil.AtomicWriteFile(remote.NexusPath(s.dir), p, 0o600); err != nil {
			log.Printf("WARN: failed to seed bootstrap nexus.json: %v", err)
		}
	}
	if p, err := json.MarshalIndent(&cfg.CertInventory, "", "  "); err == nil {
		if err := fsutil.AtomicWriteFile(remote.CertsPath(s.dir), p, 0o600); err != nil {
			log.Printf("WARN: failed to seed bootstrap certificates.json: %v", err)
		}
	}
	// Events not seeded from repo — they are filesystem-only.
}

// saveToRepo writes the combined nexus+certs blob to the encrypted repo.
func (s *bootstrapRemoteStorage) saveToRepo(ctx context.Context, nexus remote.NexusConfig, certs remote.CertInventory) error {
	if s.repo == nil {
		return nil
	}
	combined := struct {
		remote.NexusConfig
		remote.CertInventory
	}{nexus, certs}
	payload, err := json.MarshalIndent(&combined, "", "  ")
	if err != nil {
		return err
	}
	if err := s.repo.SaveConfig(ctx, persistence.RemoteConfig{Payload: payload}); err != nil {
		if errors.Is(err, persistence.ErrLocked) {
			return remote.ErrLocked
		}
		return err
	}
	return nil
}

func (s *bootstrapRemoteStorage) SaveNexus(ctx context.Context, nexus remote.NexusConfig, certs remote.CertInventory) error {
	if s == nil {
		return errors.New("remote storage: unavailable")
	}
	if !s.isMounted() {
		return remote.ErrLocked
	}
	// Attempt repo first — non-lock errors (e.g., ErrNotLeader) must fail
	// without writing the filesystem to avoid orphaned split files.
	repoErr := s.saveToRepo(ctx, nexus, certs)
	if repoErr != nil && !errors.Is(repoErr, remote.ErrLocked) {
		return repoErr
	}
	// Repo succeeded or is locked — write filesystem (always available when mounted).
	payload, err := json.MarshalIndent(&nexus, "", "  ")
	if err != nil {
		return err
	}
	_ = os.MkdirAll(s.dir, 0o700)
	if err := fsutil.AtomicWriteFile(remote.NexusPath(s.dir), payload, 0o600); err != nil {
		return err
	}
	if repoErr != nil {
		log.Printf("INFO: remote: nexus.json written to filesystem; repo locked, will sync after unlock")
	}
	return nil
}

func (s *bootstrapRemoteStorage) SaveCerts(ctx context.Context, nexus remote.NexusConfig, certs remote.CertInventory) error {
	if s == nil {
		return errors.New("remote storage: unavailable")
	}
	if !s.isMounted() {
		return remote.ErrLocked
	}
	// Attempt repo first — non-lock errors (e.g., ErrNotLeader) must fail
	// without writing the filesystem to avoid orphaned split files.
	repoErr := s.saveToRepo(ctx, nexus, certs)
	if repoErr != nil && !errors.Is(repoErr, remote.ErrLocked) {
		return repoErr
	}
	// Repo succeeded or is locked — write filesystem (always available when mounted).
	payload, err := json.MarshalIndent(&certs, "", "  ")
	if err != nil {
		return err
	}
	_ = os.MkdirAll(s.dir, 0o700)
	if err := fsutil.AtomicWriteFile(remote.CertsPath(s.dir), payload, 0o600); err != nil {
		return err
	}
	if repoErr != nil {
		log.Printf("INFO: remote: certificates.json written to filesystem; repo locked, will sync after unlock")
	}
	return nil
}

func (s *bootstrapRemoteStorage) SaveEvents(_ context.Context, events remote.EventLog) error {
	if s == nil {
		return errors.New("remote storage: unavailable")
	}
	if !s.isMounted() {
		return nil // events are best-effort
	}
	payload, err := json.MarshalIndent(&events, "", "  ")
	if err != nil {
		return err
	}
	_ = os.MkdirAll(s.dir, 0o700)
	return fsutil.AtomicWriteFile(remote.EventsPath(s.dir), payload, 0o600)
}

func (s *bootstrapRemoteStorage) isMounted() bool {
	if s == nil || strings.TrimSpace(s.root) == "" {
		return false
	}
	info, err := os.Stat(s.root)
	if err != nil {
		return false
	}
	return info.IsDir()
}
