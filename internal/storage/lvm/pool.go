package lvm

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"piccolod/internal/events"
	"piccolod/internal/runner"
)

// PoolManager manages the LVM thin pool lifecycle.
type PoolManager struct {
	run runner.CommandRunner
	bus *events.Bus
	cfg ThinPoolConfig

	mu              sync.Mutex
	lastThreshold   int
	monitorCancel   context.CancelFunc
}

// NewPoolManager creates a pool manager with the given runner and event bus.
func NewPoolManager(run runner.CommandRunner, bus *events.Bus, cfg ThinPoolConfig) *PoolManager {
	defaults := DefaultThinPoolConfig()
	if cfg.VGName == "" {
		cfg.VGName = defaults.VGName
	}
	if cfg.PoolName == "" {
		cfg.PoolName = defaults.PoolName
	}
	if cfg.ExtentPct == 0 {
		cfg.ExtentPct = defaults.ExtentPct
	}
	return &PoolManager{
		run: run,
		bus: bus,
		cfg: cfg,
	}
}

// CreatePool creates the VG and thin pool on the given device.
// Steps: [VG deactivate if leftover] → wipefs → pvcreate → vgcreate → lvcreate --type thin-pool
func (p *PoolManager) CreatePool(ctx context.Context, device string) error {
	// Clean leftover signatures (LVM, LUKS, filesystem) from prior installs.
	// Only deactivate our VG if it actually lives on this device (pvs check),
	// so we never touch an unrelated VG on another disk.
	if vg := p.pvVGName(ctx, device); vg == p.cfg.VGName {
		if err := p.run.Run(ctx, "vgchange", "-an", p.cfg.VGName); err != nil {
			return fmt.Errorf("deactivate leftover VG %s on %s: %w", p.cfg.VGName, device, err)
		}
	}
	if err := p.run.Run(ctx, "wipefs", "-a", device); err != nil {
		return fmt.Errorf("wipefs %s: %w", device, err)
	}

	// Initialize the physical volume.
	if err := p.run.Run(ctx, "pvcreate", "-f", device); err != nil {
		return fmt.Errorf("pvcreate %s: %w", device, err)
	}

	// Create the volume group.
	if err := p.run.Run(ctx, "vgcreate", p.cfg.VGName, device); err != nil {
		return fmt.Errorf("vgcreate %s on %s: %w", p.cfg.VGName, device, err)
	}

	// Create the thin pool using a percentage of the VG.
	extentArg := fmt.Sprintf("%d%%VG", p.cfg.ExtentPct)

	args := []string{
		"--type", "thin-pool",
		"--name", p.cfg.PoolName,
		"-l", extentArg,
		"--chunksize", "64k",
		p.cfg.VGName,
	}

	if err := p.run.Run(ctx, "lvcreate", args...); err != nil {
		return fmt.Errorf("lvcreate thin-pool: %w", err)
	}

	// Set error_if_no_space post-creation via lvchange (lvcreate rejects
	// --errorwhenfull for thin pools on some LVM versions).
	if p.cfg.ErrorOnFull {
		poolPath := fmt.Sprintf("%s/%s", p.cfg.VGName, p.cfg.PoolName)
		if err := p.run.Run(ctx, "lvchange", "--errorwhenfull", "y", poolPath); err != nil {
			log.Printf("WARN: lvchange --errorwhenfull failed for %s: %v", poolPath, err)
		}
	}

	log.Printf("LVM thin pool created: %s/%s on %s (%d%% VG)", p.cfg.VGName, p.cfg.PoolName, device, p.cfg.ExtentPct)
	return nil
}

// ActivatePool activates the VG and thin pool.
// Uses --partial to allow degraded activation if a USB PV is missing.
func (p *PoolManager) ActivatePool(ctx context.Context) error {
	if err := p.run.Run(ctx, "vgchange", "-ay", "--partial", p.cfg.VGName); err != nil {
		return fmt.Errorf("vgchange -ay %s: %w", p.cfg.VGName, err)
	}
	log.Printf("LVM VG activated: %s", p.cfg.VGName)
	return nil
}

// DeactivatePool deactivates the VG.
func (p *PoolManager) DeactivatePool(ctx context.Context) error {
	if err := p.run.Run(ctx, "vgchange", "-an", p.cfg.VGName); err != nil {
		return fmt.Errorf("vgchange -an %s: %w", p.cfg.VGName, err)
	}
	log.Printf("LVM VG deactivated: %s", p.cfg.VGName)
	return nil
}

// ExpandPool resizes the PV and thin pool to use any additional space on the
// underlying device. This is needed when the data partition has been expanded
// (e.g., image deployed to a larger disk, VM disk resize).
//
// Steps:
//  1. Guard: skip if VG is degraded (missing PVs from --partial activation)
//  2. pvresize <device> — PV picks up new partition size (idempotent no-op)
//  3. lvextend thin pool to cfg.ExtentPct%VG — tolerates "matches existing
//     size" for steady-state boots where pool is already at target
//
// All operations are online-safe and idempotent.
func (p *PoolManager) ExpandPool(ctx context.Context, device string) error {
	// Guard: skip expansion if VG is degraded. pvresize triggers VG metadata
	// writes; doing this with missing PVs risks metadata inconsistency.
	out, err := p.run.RunWithOutput(ctx, "vgs",
		"--noheadings", "-o", "vg_missing_pv_count",
		p.cfg.VGName,
	)
	if err != nil {
		return fmt.Errorf("vgs missing PV check: %w", err)
	}
	missingCount, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return fmt.Errorf("parse vg_missing_pv_count %q: %w", strings.TrimSpace(string(out)), err)
	}
	if missingCount > 0 {
		log.Printf("WARN: VG %s has %d missing PV(s) — skipping pool expansion", p.cfg.VGName, missingCount)
		return nil
	}

	// Resize PV to use full partition. Idempotent: no-op if PV already matches.
	if err := p.run.Run(ctx, "pvresize", device); err != nil {
		return fmt.Errorf("pvresize %s: %w", device, err)
	}

	// Extend thin pool to configured percentage of VG. The pool was created at
	// ExtentPct%VG and the VG only grows, so this target is always >= current.
	// On steady-state boots the pool is already at 95%VG; lvextend reports
	// "matches existing size" — treat that as a no-op, not an error.
	extentArg := fmt.Sprintf("%d%%VG", p.cfg.ExtentPct)
	poolPath := fmt.Sprintf("%s/%s", p.cfg.VGName, p.cfg.PoolName)
	if out, err := p.run.RunWithOutput(ctx, "lvextend", "-l", extentArg, poolPath); err != nil {
		errMsg := string(out) + err.Error()
		if strings.Contains(errMsg, "matches existing size") || strings.Contains(errMsg, "not larger") {
			log.Printf("LVM thin pool %s already at target size (%d%% VG)", poolPath, p.cfg.ExtentPct)
			return nil
		}
		return fmt.Errorf("lvextend %s to %s: %w", poolPath, extentArg, err)
	}

	log.Printf("LVM thin pool expanded: %s", poolPath)
	return nil
}

// PoolStatus returns the current thin pool utilization.
func (p *PoolManager) PoolStatus(ctx context.Context) (PoolStats, error) {
	// lvs --noheadings --nosuffix --units b -o data_percent,metadata_percent,lv_size
	out, err := p.run.RunWithOutput(ctx, "lvs",
		"--noheadings", "--nosuffix", "--units", "b",
		"-o", "data_percent,metadata_percent,lv_size",
		fmt.Sprintf("%s/%s", p.cfg.VGName, p.cfg.PoolName),
	)
	if err != nil {
		return PoolStats{}, fmt.Errorf("lvs pool status: %w", err)
	}

	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) < 3 {
		return PoolStats{}, fmt.Errorf("unexpected lvs output: %q", string(out))
	}

	var stats PoolStats
	var err2 error
	if stats.DataPercent, err2 = strconv.ParseFloat(fields[0], 64); err2 != nil {
		return PoolStats{}, fmt.Errorf("parse data_percent %q: %w", fields[0], err2)
	}
	if stats.MetadataPercent, err2 = strconv.ParseFloat(fields[1], 64); err2 != nil {
		return PoolStats{}, fmt.Errorf("parse metadata_percent %q: %w", fields[1], err2)
	}
	totalBytes, err2 := strconv.ParseFloat(fields[2], 64)
	if err2 != nil {
		return PoolStats{}, fmt.Errorf("parse lv_size %q: %w", fields[2], err2)
	}
	stats.TotalDataBytes = int64(totalBytes)
	stats.UsedDataBytes = int64(totalBytes * stats.DataPercent / 100.0)

	return stats, nil
}

// pvVGName returns the VG name that owns a PV device, or "" if the device
// is not a PV (or on any error). Used to scope VG deactivation to the
// target device only.
func (p *PoolManager) pvVGName(ctx context.Context, device string) string {
	out, err := p.run.RunWithOutput(ctx, "pvs", "--noheadings", "-o", "vg_name", device)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// VGExists checks if the volume group exists.
// Returns (true, nil) if the VG exists, (false, nil) if it does not,
// or (false, err) if the check itself failed (I/O error, missing binary).
func (p *PoolManager) VGExists(ctx context.Context) (bool, error) {
	err := p.run.Run(ctx, "vgs", "--noheadings", p.cfg.VGName)
	if err == nil {
		return true, nil
	}
	// vgs exits with code 5 for "volume group not found".
	// Only treat that as "does not exist". Propagate all other errors
	// to prevent callers from mistakenly falling back to destructive init.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 5 {
		return false, nil
	}
	return false, fmt.Errorf("vgs check %s: %w", p.cfg.VGName, err)
}

// StartHealthMonitor begins periodic thin pool health monitoring.
// Publishes events at threshold crossings (80%, 90%, 95%).
// The monitor lifecycle is managed by StopHealthMonitor, not by the caller's
// context — so we derive from context.Background() to avoid premature
// cancellation when called from a request handler.
func (p *PoolManager) StartHealthMonitor(interval time.Duration) {
	p.mu.Lock()
	if p.monitorCancel != nil {
		p.mu.Unlock()
		return
	}
	monCtx, cancel := context.WithCancel(context.Background())
	p.monitorCancel = cancel
	p.mu.Unlock()

	go p.healthMonitorLoop(monCtx, interval)
}

// StopHealthMonitor stops the health monitor.
func (p *PoolManager) StopHealthMonitor() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.monitorCancel != nil {
		p.monitorCancel()
		p.monitorCancel = nil
	}
}

func (p *PoolManager) healthMonitorLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.checkPoolHealth(ctx)
		}
	}
}

func (p *PoolManager) checkPoolHealth(ctx context.Context) {
	stats, err := p.PoolStatus(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return // shutting down — suppress log noise
		}
		log.Printf("WARN: thin pool health check failed: %v", err)
		return
	}

	level := stats.ThresholdLevel()
	p.mu.Lock()
	prev := p.lastThreshold
	p.lastThreshold = level // always track actual level (allows re-alerting after recovery)
	p.mu.Unlock()

	// Publish when crossing a new threshold upward.
	if level > prev && level > 0 && p.bus != nil {
		log.Printf("WARN: thin pool data usage at %.1f%% (threshold %d%%)", stats.DataPercent, level)
		p.bus.Publish(events.Event{
			Topic: events.TopicStorageEmergency,
			Payload: events.StorageEmergencyEvent{
				Level: "soft",
				Error: fmt.Sprintf("thin pool data usage %.1f%% exceeds %d%% threshold", stats.DataPercent, level),
			},
		})
	}
}
