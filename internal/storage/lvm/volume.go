package lvm

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"

	"piccolod/internal/runner"
)

// LVManager manages thin logical volumes within a thin pool.
type LVManager struct {
	run    runner.CommandRunner
	vgName string
	pool   string
}

// LVInventoryEntry is the exact physical identity captured for one LV.
// UUID distinguishes an LV that was removed and recreated under the same
// name after a broad inventory was collected.
type LVInventoryEntry struct {
	UUID     string
	PoolName string
	Active   bool
}

// NewLVManager creates an LV manager for the given VG and thin pool.
func NewLVManager(run runner.CommandRunner, vgName, poolName string) *LVManager {
	if vgName == "" {
		vgName = DefaultVGName
	}
	if poolName == "" {
		poolName = DefaultThinPoolName
	}
	return &LVManager{
		run:    run,
		vgName: vgName,
		pool:   poolName,
	}
}

// CreateThinLV creates a new thin logical volume.
// sizeBytes is rounded up to the nearest 512-byte sector boundary
// (LVM requires sector-aligned --virtualsize values).
func (m *LVManager) CreateThinLV(ctx context.Context, name string, sizeBytes int64) error {
	sizeBytes = alignToSector(sizeBytes)
	sizeArg := fmt.Sprintf("%dB", sizeBytes)
	if err := m.run.Run(ctx, "lvcreate",
		"--thin",
		"--name", name,
		"--virtualsize", sizeArg,
		fmt.Sprintf("%s/%s", m.vgName, m.pool),
	); err != nil {
		return fmt.Errorf("lvcreate thin LV %s: %w", name, err)
	}
	log.Printf("thin LV created: %s/%s (%d bytes)", m.vgName, name, sizeBytes)
	return nil
}

// RemoveThinLV removes a thin logical volume.
func (m *LVManager) RemoveThinLV(ctx context.Context, name string) error {
	lvPath := fmt.Sprintf("%s/%s", m.vgName, name)
	if err := m.run.Run(ctx, "lvremove", "-f", lvPath); err != nil {
		return fmt.Errorf("lvremove %s: %w", lvPath, err)
	}
	log.Printf("thin LV removed: %s", lvPath)
	return nil
}

// ActivateLV activates a thin logical volume.
func (m *LVManager) ActivateLV(ctx context.Context, name string) error {
	lvPath := fmt.Sprintf("%s/%s", m.vgName, name)
	if err := m.run.Run(ctx, "lvchange", "-ay", lvPath); err != nil {
		return fmt.Errorf("lvchange -ay %s: %w", lvPath, err)
	}
	if err := m.run.Run(ctx, "udevadm", "settle", "--timeout=5"); err != nil {
		return fmt.Errorf("udevadm settle after lvchange -ay %s: %w", lvPath, err)
	}
	return nil
}

// DeactivateLV deactivates a thin logical volume.
func (m *LVManager) DeactivateLV(ctx context.Context, name string) error {
	lvPath := fmt.Sprintf("%s/%s", m.vgName, name)
	if err := m.run.Run(ctx, "lvchange", "-an", lvPath); err != nil {
		return fmt.Errorf("lvchange -an %s: %w", lvPath, err)
	}
	return nil
}

// CreateSnapshot creates a thin snapshot of an existing thin LV.
// The snapshot is created inactive. Caller must ActivateLV() explicitly.
//
// Precondition: the thin pool must be active (PoolManager.ActivatePool).
// The origin LV does NOT need to be active — thin snapshots are metadata ops.
func (m *LVManager) CreateSnapshot(ctx context.Context, originLV, snapshotName string) error {
	originPath := fmt.Sprintf("%s/%s", m.vgName, originLV)
	if err := m.run.Run(ctx, "lvcreate",
		"--snapshot",
		"--name", snapshotName,
		"--setactivationskip", "n",
		originPath,
	); err != nil {
		return fmt.Errorf("lvcreate snapshot %s from %s: %w", snapshotName, originPath, err)
	}
	log.Printf("thin snapshot created: %s/%s (origin: %s)", m.vgName, snapshotName, originLV)
	return nil
}

// ResizeLV resizes a thin logical volume.
func (m *LVManager) ResizeLV(ctx context.Context, name string, newSizeBytes int64) error {
	newSizeBytes = alignToSector(newSizeBytes)
	sizeArg := fmt.Sprintf("%dB", newSizeBytes)
	lvPath := fmt.Sprintf("%s/%s", m.vgName, name)
	if err := m.run.Run(ctx, "lvresize", "-L", sizeArg, lvPath); err != nil {
		return fmt.Errorf("lvresize %s to %s: %w", lvPath, sizeArg, err)
	}
	log.Printf("thin LV resized: %s → %d bytes", lvPath, newSizeBytes)
	return nil
}

// LVSizeBytes returns the current logical size of an LV.
func (m *LVManager) LVSizeBytes(ctx context.Context, name string) (int64, error) {
	lvPath := fmt.Sprintf("%s/%s", m.vgName, name)
	out, err := m.run.RunWithOutput(ctx, "lvs",
		"--noheadings", "--nosuffix", "--units", "b",
		"-o", "lv_size",
		lvPath,
	)
	if err != nil {
		return 0, fmt.Errorf("lvs size %s: %w", lvPath, err)
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) == 0 {
		return 0, fmt.Errorf("lvs size %s: empty output", lvPath)
	}
	sizeBytes, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, fmt.Errorf("parse lv_size %q for %s: %w", fields[0], lvPath, err)
	}
	return alignToSector(int64(sizeBytes)), nil
}

// EnsureLVSizeAtLeast grows a thin LV to at least minSizeBytes and never
// shrinks an LV that is already larger. It returns the size the caller should
// treat as the converged LV size.
func (m *LVManager) EnsureLVSizeAtLeast(ctx context.Context, name string, minSizeBytes int64) (int64, error) {
	minSizeBytes = alignToSector(minSizeBytes)
	currentSize, err := m.LVSizeBytes(ctx, name)
	if err != nil {
		return 0, err
	}
	if currentSize >= minSizeBytes {
		log.Printf("thin LV already at or above target: %s/%s current=%d target=%d", m.vgName, name, currentSize, minSizeBytes)
		return currentSize, nil
	}
	if err := m.ResizeLV(ctx, name, minSizeBytes); err != nil {
		return 0, err
	}
	return minSizeBytes, nil
}

// RenameLV renames a thin logical volume.
func (m *LVManager) RenameLV(ctx context.Context, oldName, newName string) error {
	if err := m.run.Run(ctx, "lvrename", m.vgName, oldName, newName); err != nil {
		return fmt.Errorf("lvrename %s/%s → %s: %w", m.vgName, oldName, newName, err)
	}
	log.Printf("thin LV renamed: %s/%s → %s", m.vgName, oldName, newName)
	return nil
}

// LVExists checks if a thin LV exists.
func (m *LVManager) LVExists(ctx context.Context, name string) bool {
	lvPath := fmt.Sprintf("%s/%s", m.vgName, name)
	err := m.run.Run(ctx, "lvs", "--noheadings", lvPath)
	return err == nil
}

// LVExistsExact returns authoritative existence for one LV name. Unlike
// ListLVs, this probe enumerates every LV in the VG and treats any unexpected
// row shape as ambiguous instead of silently filtering it out. Destructive
// callers use it to prove physical absence before deleting ownership metadata.
func (m *LVManager) LVExistsExact(ctx context.Context, name string) (bool, error) {
	_, exists, err := m.InspectLVExact(ctx, name)
	return exists, err
}

const strictLVSeparator = "|"

func (m *LVManager) strictInventory(
	ctx context.Context,
	selectName string,
) (map[string]LVInventoryEntry, error) {
	args := []string{
		"--noheadings",
		"--separator", strictLVSeparator,
		"--unquoted",
		"-o", "lv_name,lv_uuid,lv_attr,pool_lv",
	}
	if selectName != "" {
		args = append(args, "--select", "lv_name="+selectName)
	}
	args = append(args, m.vgName)
	out, err := m.run.RunWithOutput(ctx, "lvs", args...)
	if err != nil {
		if selectName != "" {
			return nil, fmt.Errorf("lvs exact identity %s/%s: %w", m.vgName, selectName, err)
		}
		return nil, fmt.Errorf("lvs strict inventory %s: %w", m.vgName, err)
	}

	inventory := make(map[string]LVInventoryEntry)
	uuids := make(map[string]string)
	for lineNumber, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, strictLVSeparator)
		if len(fields) != 4 {
			return nil, fmt.Errorf("lvs strict inventory %s: malformed row %d: %q", m.vgName, lineNumber+1, line)
		}
		for index := range fields {
			fields[index] = strings.TrimSpace(fields[index])
		}
		name, uuid, attr, poolName := fields[0], fields[1], fields[2], fields[3]
		if name == "" || uuid == "" || attr == "" {
			return nil, fmt.Errorf("lvs strict inventory %s: incomplete row %d: %q", m.vgName, lineNumber+1, line)
		}
		if len(attr) != 10 {
			return nil, fmt.Errorf(
				"lvs strict inventory %s: malformed lv_attr %q for %q",
				m.vgName,
				attr,
				name,
			)
		}
		if selectName != "" && name != selectName {
			return nil, fmt.Errorf(
				"lvs exact identity %s/%s: unexpected identity %q",
				m.vgName,
				selectName,
				name,
			)
		}
		if _, duplicate := inventory[name]; duplicate {
			return nil, fmt.Errorf("lvs strict inventory %s: duplicate LV name %q", m.vgName, name)
		}
		if priorName, duplicate := uuids[uuid]; duplicate {
			return nil, fmt.Errorf(
				"lvs strict inventory %s: duplicate LV UUID %q for %q and %q",
				m.vgName,
				uuid,
				priorName,
				name,
			)
		}
		var active bool
		switch attr[4] {
		case 'a':
			active = true
		case '-':
			active = false
		default:
			return nil, fmt.Errorf(
				"lvs strict inventory %s: ambiguous activation state %q in lv_attr %q for %q",
				m.vgName,
				attr[4],
				attr,
				name,
			)
		}
		inventory[name] = LVInventoryEntry{
			UUID:     uuid,
			PoolName: poolName,
			Active:   active,
		}
		uuids[uuid] = name
	}
	return inventory, nil
}

// StrictLVInventory returns one fail-closed broad inventory for a maintenance
// pass. Callers must treat it as discovery only and refresh exact ownership
// before destructive work.
func (m *LVManager) StrictLVInventory(ctx context.Context) (map[string]LVInventoryEntry, error) {
	return m.strictInventory(ctx, "")
}

// InspectLVExact performs a strict targeted identity proof for one LV name.
// An empty successful result proves absence; malformed, duplicate, or
// unexpected rows fail closed.
func (m *LVManager) InspectLVExact(ctx context.Context, name string) (LVInventoryEntry, bool, error) {
	inventory, err := m.strictInventory(ctx, name)
	if err != nil {
		return LVInventoryEntry{}, false, err
	}
	if len(inventory) == 0 {
		return LVInventoryEntry{}, false, nil
	}
	return inventory[name], true, nil
}

// LVPath returns the device path for a thin LV.
func (m *LVManager) LVPath(name string) string {
	return fmt.Sprintf("/dev/%s/%s", m.vgName, name)
}

// ListLVs returns all thin LVs in the pool.
func (m *LVManager) ListLVs(ctx context.Context) ([]ThinLV, error) {
	// lvs --noheadings --nosuffix --units b -o lv_name,lv_size,lv_attr
	// Filter to only thin volumes in our pool.
	out, err := m.run.RunWithOutput(ctx, "lvs",
		"--noheadings", "--nosuffix", "--units", "b",
		"-o", "lv_name,lv_size,lv_attr,pool_lv",
		m.vgName,
	)
	if err != nil {
		return nil, fmt.Errorf("lvs list: %w", err)
	}

	var lvs []ThinLV
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		name := fields[0]
		poolLV := fields[3]

		// Only include LVs that belong to our thin pool.
		if poolLV != m.pool {
			continue
		}

		sizeBytes, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			continue // skip unparseable lines
		}
		attr := fields[2]
		// LV attr field: character at index 4 (5th position) is device state: 'a' = active.
		active := len(attr) > 4 && attr[4] == 'a'

		lvs = append(lvs, ThinLV{
			Name:      name,
			VGName:    m.vgName,
			PoolName:  m.pool,
			SizeBytes: int64(sizeBytes),
			Active:    active,
			Path:      m.LVPath(name),
		})
	}
	return lvs, nil
}

// alignToSector rounds n up to the nearest 512-byte boundary.
func alignToSector(n int64) int64 {
	const sectorSize = 512
	if n <= 0 {
		return 0
	}
	return (n + sectorSize - 1) &^ (sectorSize - 1)
}
