// Package workspacedisk implements container-independent persistence for workspace mode apps.
// It manages the persistent workspace disk that survives container recreation, enabling
// safe listener CRUD and failover without data loss.
//
// The workspace disk uses an overlay filesystem to combine a shared base image (lowerdir)
// with a per-workspace writable layer (upperdir), storing all mutable bytes inside the
// per-app encrypted volume.
package workspacedisk

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"piccolod/internal/fsutil"
)

// MetaFormatVersion is the current schema version for meta.json files.
const MetaFormatVersion = "1"

// WorkspaceMeta holds the workspace disk metadata stored in meta.json.
// This file lives at {volume}/disk/workspace/meta.json and contains all
// information needed to reconstruct the container wrapper from the workspace disk.
//
// Schema follows RFC §5.2.1 - fields stored in OCI format for fidelity.
type WorkspaceMeta struct {
	// FormatVersion identifies the schema version (currently "1").
	FormatVersion string `json:"format_version"`

	// BaseImageDigest is the canonical digest of the base image used as lowerdir.
	// Example: "docker.io/library/ubuntu@sha256:..."
	BaseImageDigest string `json:"base_image_digest"`

	// BaseImageRef is the user-facing reference (for UI/auditing).
	// Example: "ubuntu:22.04"
	BaseImageRef string `json:"base_image_ref"`

	// ImageConfig contains the OCI image configuration needed for --rootfs mode.
	// When running with podman --rootfs, Podman doesn't apply image config automatically,
	// so we must persist and apply it ourselves.
	ImageConfig ImageConfig `json:"image_config"`

	// CreatedAt is the timestamp when the workspace disk was initialized.
	CreatedAt time.Time `json:"created_at"`
}

// ImageConfig holds OCI image configuration fields needed for container creation.
// These are extracted from the base image at install time and applied when
// creating the container wrapper with --rootfs.
type ImageConfig struct {
	// Entrypoint is the OCI entrypoint (may be empty).
	Entrypoint []string `json:"entrypoint,omitempty"`

	// Cmd is the OCI command (may be empty).
	Cmd []string `json:"cmd,omitempty"`

	// Env is the OCI environment in KEY=VALUE format.
	Env []string `json:"env,omitempty"`

	// WorkingDir is the working directory for the container.
	WorkingDir string `json:"workdir,omitempty"`

	// User is the user/group to run as (format: "uid:gid" or "user:group").
	User string `json:"user,omitempty"`
}

// Validate checks that required fields are present and valid.
func (m *WorkspaceMeta) Validate() error {
	if m.FormatVersion == "" {
		return fmt.Errorf("format_version is required")
	}
	if m.FormatVersion != MetaFormatVersion {
		return fmt.Errorf("unsupported format_version: %s (expected %s)", m.FormatVersion, MetaFormatVersion)
	}
	if m.BaseImageDigest == "" {
		return fmt.Errorf("base_image_digest is required")
	}
	if m.CreatedAt.IsZero() {
		return fmt.Errorf("created_at is required")
	}
	return nil
}

// BuildOriginalCommand constructs the original container command from image config.
// This combines entrypoint and cmd into a single slice suitable for execution.
func (ic *ImageConfig) BuildOriginalCommand() []string {
	var cmd []string
	cmd = append(cmd, ic.Entrypoint...)
	cmd = append(cmd, ic.Cmd...)
	if len(cmd) == 0 {
		// Fallback for images without explicit entrypoint/cmd
		cmd = []string{"/bin/sh"}
	}
	return cmd
}

// MetaFilename is the standard name for the metadata file.
const MetaFilename = "meta.json"

// LoadMeta reads and parses meta.json from the workspace directory.
func LoadMeta(workspaceDir string) (*WorkspaceMeta, error) {
	path := filepath.Join(workspaceDir, MetaFilename)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("workspace meta not found: %w", err)
		}
		return nil, fmt.Errorf("read workspace meta: %w", err)
	}

	var meta WorkspaceMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("parse workspace meta: %w", err)
	}

	if err := meta.Validate(); err != nil {
		return nil, fmt.Errorf("invalid workspace meta: %w", err)
	}

	return &meta, nil
}

// SaveMeta writes meta.json to the workspace directory atomically.
// Uses fsutil.AtomicWriteFile to prevent corruption during interruption
// (e.g., VM save state or unexpected shutdown).
func SaveMeta(workspaceDir string, meta *WorkspaceMeta) error {
	if err := meta.Validate(); err != nil {
		return fmt.Errorf("invalid workspace meta: %w", err)
	}

	path := filepath.Join(workspaceDir, MetaFilename)
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal workspace meta: %w", err)
	}

	if err := fsutil.AtomicWriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write workspace meta: %w", err)
	}

	return nil
}

// MetaExists checks if meta.json exists in the workspace directory.
func MetaExists(workspaceDir string) bool {
	path := filepath.Join(workspaceDir, MetaFilename)
	_, err := os.Stat(path)
	return err == nil
}
