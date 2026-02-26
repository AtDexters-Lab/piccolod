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
func (m *LVManager) CreateThinLV(ctx context.Context, name string, sizeBytes int64) error {
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

// ResizeLV resizes a thin logical volume.
func (m *LVManager) ResizeLV(ctx context.Context, name string, newSizeBytes int64) error {
	sizeArg := fmt.Sprintf("%dB", newSizeBytes)
	lvPath := fmt.Sprintf("%s/%s", m.vgName, name)
	if err := m.run.Run(ctx, "lvresize", "-L", sizeArg, lvPath); err != nil {
		return fmt.Errorf("lvresize %s to %s: %w", lvPath, sizeArg, err)
	}
	log.Printf("thin LV resized: %s → %d bytes", lvPath, newSizeBytes)
	return nil
}

// LVExists checks if a thin LV exists.
func (m *LVManager) LVExists(ctx context.Context, name string) bool {
	lvPath := fmt.Sprintf("%s/%s", m.vgName, name)
	err := m.run.Run(ctx, "lvs", "--noheadings", lvPath)
	return err == nil
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
