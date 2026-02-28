// Package vdo manages dm-vdo (Virtual Data Optimizer) targets for
// transparent inline compression on block devices.
//
// VDO is placed above LUKS in the device stack, operating on plaintext
// data for maximum compression ratios. Lifecycle:
//
//	vdoformat <device>                    → initialize on-disk superblock
//	dmsetup create <name> --table "..."   → activate dm-vdo target
//	dmsetup remove <name>                 → deactivate dm-vdo target
package vdo

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"

	"piccolod/internal/runner"
)

// Manager provides dm-vdo target lifecycle operations.
type Manager struct {
	run runner.CommandRunner
}

// NewManager creates a VDO manager using the given command runner.
func NewManager(run runner.CommandRunner) *Manager {
	return &Manager{run: run}
}

// TargetParams holds dm-vdo kernel target parameters. These must be
// consistent between the golden LV and all snapshots derived from it.
// Persisted in volumeMetaV3.VDOParams.
type TargetParams struct {
	LogicalSizeBytes int64 // virtual device size in bytes (= physical for 1:1)
	MinIOSize        int   // minimum I/O size: 512 or 4096
	BlockMapCacheKB  int   // block map cache in KiB (minimum 128 MiB = 131072)
	BlockMapEraLen   int   // 1–16380 (default 16380)
}

// DefaultTargetParams returns sensible defaults for a given logical size.
func DefaultTargetParams(logicalSizeBytes int64) TargetParams {
	return TargetParams{
		LogicalSizeBytes: logicalSizeBytes,
		MinIOSize:        4096,
		BlockMapCacheKB:  131072, // 128 MiB
		BlockMapEraLen:   16380,
	}
}

// Format initializes the VDO superblock on a backing device.
// The device should already be LUKS-opened (plaintext).
// vdoformat does not take a logical size — that is a dm target property.
func (m *Manager) Format(ctx context.Context, device string) error {
	if err := m.run.Run(ctx, "vdoformat", "--force", device); err != nil {
		return fmt.Errorf("vdoformat %s: %w", device, err)
	}
	log.Printf("VDO formatted: %s", device)
	return nil
}

// Create activates a dm-vdo target with the given name and parameters.
// The backing device must have a VDO superblock (from Format or snapshot).
//
// dm-vdo table format (kernel 6.9+, V4):
//
//	0 <logical_sectors> vdo V4 <backing> <storage_4k> <min_io>
//	  <block_map_cache_4k> <block_map_era_len> compression on deduplication off
func (m *Manager) Create(ctx context.Context, dmName, backingDevice string, params TargetParams) error {
	// Query backing device size for storage_4k_blocks.
	storageBytes, err := m.deviceSize(ctx, backingDevice)
	if err != nil {
		return fmt.Errorf("query backing device size: %w", err)
	}

	logicalSectors := params.LogicalSizeBytes / 512
	storage4KBlocks := storageBytes / 4096
	blockMapCache4K := int64(params.BlockMapCacheKB) * 1024 / 4096

	table := fmt.Sprintf("0 %d vdo V4 %s %d %d %d %d compression on deduplication off",
		logicalSectors,
		backingDevice,
		storage4KBlocks,
		params.MinIOSize,
		blockMapCache4K,
		params.BlockMapEraLen,
	)

	if err := m.run.Run(ctx, "dmsetup", "create", dmName, "--table", table); err != nil {
		return fmt.Errorf("dmsetup create %s: %w", dmName, err)
	}
	log.Printf("VDO target created: %s (backing=%s)", dmName, backingDevice)
	return nil
}

// Remove deactivates a dm-vdo target.
func (m *Manager) Remove(ctx context.Context, dmName string) error {
	if err := m.run.Run(ctx, "dmsetup", "remove", dmName); err != nil {
		return fmt.Errorf("dmsetup remove %s: %w", dmName, err)
	}
	log.Printf("VDO target removed: %s", dmName)
	return nil
}

// Exists checks if a dm-vdo target is active.
func (m *Manager) Exists(ctx context.Context, dmName string) bool {
	return m.run.Run(ctx, "dmsetup", "info", dmName) == nil
}

// DevicePath returns the /dev/mapper path for a dm-vdo target.
func DevicePath(dmName string) string {
	return "/dev/mapper/" + dmName
}

// CheckAvailability verifies that vdoformat is installed and the kernel
// dm-vdo target is loaded. Returns an error with actionable guidance
// if either is missing.
func (m *Manager) CheckAvailability(ctx context.Context) error {
	// Check vdoformat binary.
	if err := m.run.Run(ctx, "which", "vdoformat"); err != nil {
		return fmt.Errorf("vdoformat not found: install the 'vdo' package (zypper in vdo)")
	}

	// Check kernel dm-vdo target.
	out, err := m.run.RunWithOutput(ctx, "dmsetup", "targets")
	if err != nil {
		return fmt.Errorf("dmsetup targets: %w", err)
	}
	if !strings.Contains(string(out), "vdo") {
		return fmt.Errorf("kernel dm-vdo target not loaded: run 'modprobe kvdo' or ensure kernel has CONFIG_DM_VDO=m")
	}

	return nil
}

// deviceSize returns the size of a block device in bytes.
func (m *Manager) deviceSize(ctx context.Context, device string) (int64, error) {
	out, err := m.run.RunWithOutput(ctx, "blockdev", "--getsize64", device)
	if err != nil {
		return 0, fmt.Errorf("blockdev --getsize64 %s: %w", device, err)
	}
	size, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse device size %q: %w", string(out), err)
	}
	return size, nil
}
