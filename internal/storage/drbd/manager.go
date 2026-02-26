package drbd

import (
	"context"
	"fmt"
	"log"
	"strings"

	"piccolod/internal/runner"
)

// ResourceManager manages DRBD resources. It verifies kernel module
// availability and provides resource lifecycle operations.
type ResourceManager struct {
	run     runner.CommandRunner
	metaDir string
}

// NewResourceManager creates a DRBD resource manager.
// metaDir is the directory for external metadata files; empty uses default.
func NewResourceManager(run runner.CommandRunner, metaDir string) *ResourceManager {
	if metaDir == "" {
		metaDir = DefaultMetaDir()
	}
	return &ResourceManager{
		run:     run,
		metaDir: metaDir,
	}
}

// MetaDir returns the external metadata directory.
func (m *ResourceManager) MetaDir() string {
	return m.metaDir
}

// EnsureAvailable verifies that the DRBD kernel module is loaded
// and that drbdadm/drbdsetup are available. Returns an error if
// DRBD is not usable.
func (m *ResourceManager) EnsureAvailable(ctx context.Context) error {
	// Check kernel module.
	if err := m.run.Run(ctx, "modprobe", "drbd"); err != nil {
		return fmt.Errorf("drbd kernel module not available: %w", err)
	}

	// Check drbdadm binary.
	if err := m.run.Run(ctx, "drbdadm", "--version"); err != nil {
		return fmt.Errorf("drbdadm not available: %w", err)
	}

	// Check drbdsetup binary (used by ResourceStatus).
	if err := m.run.Run(ctx, "drbdsetup", "--version"); err != nil {
		return fmt.Errorf("drbdsetup not available: %w", err)
	}

	return nil
}

// DRBDVersion returns the DRBD utils version string.
func (m *ResourceManager) DRBDVersion(ctx context.Context) (string, error) {
	out, err := m.run.RunWithOutput(ctx, "drbdadm", "--version")
	if err != nil {
		return "", fmt.Errorf("drbdadm --version: %w", err)
	}
	// First line is typically "DRBDADM_VERSION=X.Y.Z"
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "DRBDADM_VERSION=") {
			return strings.TrimPrefix(line, "DRBDADM_VERSION="), nil
		}
	}
	return strings.TrimSpace(string(out)), nil
}

// ResourceStatus returns the current state of a DRBD resource.
func (m *ResourceManager) ResourceStatus(ctx context.Context, name string) (ResourceState, error) {
	out, err := m.run.RunWithOutput(ctx, "drbdsetup", "status", name, "--statistics")
	if err != nil {
		return ResourceState{}, fmt.Errorf("drbdsetup status %s: %w", name, err)
	}

	state := ResourceState{
		Name:       name,
		DevicePath: DevicePath(name),
	}

	// Parse drbdsetup status output.
	// Example: "vol-abc role:Primary disk:UpToDate"
	output := string(out)
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		for _, field := range strings.Fields(line) {
			parts := strings.SplitN(field, ":", 2)
			if len(parts) != 2 {
				continue
			}
			switch parts[0] {
			case "role":
				state.Role = parts[1]
			case "disk":
				state.DiskState = parts[1]
			case "connection":
				state.Connection = parts[1]
			}
		}
	}

	// Default connection state for single-node.
	if state.Connection == "" {
		state.Connection = "StandAlone"
	}

	return state, nil
}

// DevicePath returns the DRBD device path for a resource name.
func DevicePath(name string) string {
	return fmt.Sprintf("/dev/drbd/by-res/%s", name)
}

// ListResources returns the names of all configured DRBD resources.
func (m *ResourceManager) ListResources(ctx context.Context) ([]string, error) {
	out, err := m.run.RunWithOutput(ctx, "drbdadm", "sh-resources")
	if err != nil {
		return nil, fmt.Errorf("list drbd resources: %w", err)
	}

	var names []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name := strings.TrimSpace(line)
		if name != "" {
			names = append(names, name)
		}
	}
	return names, nil
}

// DownAll brings down all DRBD resources.
func (m *ResourceManager) DownAll(ctx context.Context) error {
	if err := m.run.Run(ctx, "drbdadm", "down", "all"); err != nil {
		log.Printf("WARN: drbdadm down all: %v", err)
		return fmt.Errorf("drbdadm down all: %w", err)
	}
	return nil
}
