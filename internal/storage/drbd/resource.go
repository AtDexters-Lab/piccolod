package drbd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"text/template"

	"piccolod/internal/runner"
)

// configTemplate generates a single-node DRBD resource configuration.
// When a second node joins, the config is regenerated with a peer section
// and a connection-mesh block (omitted here — invalid with a single host).
var configTemplate = template.Must(template.New("drbd").Parse(`resource {{.Name}} {
    options {
        auto-promote yes;
    }
    on {{.Hostname}} {
        node-id {{.NodeID}};
        disk {{.BackingDevice}};
        meta-disk {{.MetaPath}};
        device /dev/drbd/by-res/{{.Name}};
    }
}
`))

// configData holds the template parameters for DRBD resource config.
type configData struct {
	Name          string
	BackingDevice string
	MetaPath      string
	Hostname      string
	NodeID        int
}

// ResourceOps handles lifecycle operations for a single DRBD resource.
type ResourceOps struct {
	run     runner.CommandRunner
	metaDir string
	confDir string
	cfg     ResourceConfig
}

// NewResourceOps creates a resource operations handler.
// The metadata directory is resolved in order: cfg.MetaDir > metaDir param > DefaultMetaDir().
// The config directory is resolved from cfg.ConfDir, defaulting to /etc/drbd.d/.
func NewResourceOps(run runner.CommandRunner, metaDir string, cfg ResourceConfig) *ResourceOps {
	dir := cfg.MetaDir
	if dir == "" {
		dir = metaDir
	}
	if dir == "" {
		dir = DefaultMetaDir()
	}
	cDir := cfg.ConfDir
	if cDir == "" {
		cDir = "/etc/drbd.d"
	}
	return &ResourceOps{
		run:     run,
		metaDir: dir,
		confDir: cDir,
		cfg:     cfg,
	}
}

// metaPath returns the external metadata file path for this resource.
func (r *ResourceOps) metaPath() string {
	return filepath.Join(r.metaDir, r.cfg.Name+".meta")
}

// configPath returns the DRBD config file path for this resource.
func (r *ResourceOps) configPath() string {
	return filepath.Join(r.confDir, r.cfg.Name+".res")
}

// DevicePath returns the block device path for this resource.
func (r *ResourceOps) DevicePath() string {
	return DevicePath(r.cfg.Name)
}

// WriteConfig generates and writes the DRBD resource configuration atomically.
// It writes to a temp file, syncs, and renames to avoid partial configs.
func (r *ResourceOps) WriteConfig() error {
	hostname, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("get hostname: %w", err)
	}

	data := configData{
		Name:          r.cfg.Name,
		BackingDevice: r.cfg.BackingDevice,
		MetaPath:      r.metaPath(),
		Hostname:      hostname,
		NodeID:        r.cfg.NodeID,
	}

	confDir := filepath.Dir(r.configPath())

	// Ensure config directory exists.
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		return fmt.Errorf("create drbd config dir: %w", err)
	}

	// Write to temp file in same directory (ensures same filesystem for rename).
	tmp, err := os.CreateTemp(confDir, ".drbd-*.tmp")
	if err != nil {
		return fmt.Errorf("create drbd config temp file: %w", err)
	}
	tmpPath := tmp.Name()

	if err := configTemplate.Execute(tmp, data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("render drbd config: %w", err)
	}

	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("sync drbd config: %w", err)
	}

	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close drbd config temp file: %w", err)
	}

	if err := os.Rename(tmpPath, r.configPath()); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename drbd config: %w", err)
	}

	return nil
}

// CreateMetadata initializes DRBD metadata for this resource.
// Must be called once before the first Up.
func (r *ResourceOps) CreateMetadata(ctx context.Context) error {
	// Ensure metadata directory exists.
	if err := os.MkdirAll(r.metaDir, 0o700); err != nil {
		return fmt.Errorf("create drbd meta dir: %w", err)
	}

	// drbdadm create-md --force <resource>
	// --force skips the "are you sure" prompt.
	if err := r.run.Run(ctx, "drbdadm", "create-md", "--force", r.cfg.Name); err != nil {
		return fmt.Errorf("drbdadm create-md %s: %w", r.cfg.Name, err)
	}

	log.Printf("drbd: metadata created for %s", r.cfg.Name)
	return nil
}

// Up brings the resource online: attach disk, connect (standalone), promote.
func (r *ResourceOps) Up(ctx context.Context) error {
	if err := r.run.Run(ctx, "drbdadm", "up", r.cfg.Name); err != nil {
		return fmt.Errorf("drbdadm up %s: %w", r.cfg.Name, err)
	}

	// Force primary in single-node mode (no peer to negotiate with).
	if err := r.run.Run(ctx, "drbdadm", "primary", "--force", r.cfg.Name); err != nil {
		return fmt.Errorf("drbdadm primary --force %s: %w", r.cfg.Name, err)
	}

	log.Printf("drbd: resource %s up (primary, standalone)", r.cfg.Name)
	return nil
}

// Down brings the resource offline.
func (r *ResourceOps) Down(ctx context.Context) error {
	if err := r.run.Run(ctx, "drbdadm", "down", r.cfg.Name); err != nil {
		return fmt.Errorf("drbdadm down %s: %w", r.cfg.Name, err)
	}
	log.Printf("drbd: resource %s down", r.cfg.Name)
	return nil
}

// RemoveConfig removes the resource configuration and metadata files.
func (r *ResourceOps) RemoveConfig() error {
	var errs []error
	if err := os.Remove(r.configPath()); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("remove config %s: %w", r.configPath(), err))
	}
	if err := os.Remove(r.metaPath()); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("remove metadata %s: %w", r.metaPath(), err))
	}
	return errors.Join(errs...)
}
