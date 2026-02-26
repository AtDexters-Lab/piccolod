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
// Steps: pvcreate → vgcreate → lvcreate --type thin-pool
func (p *PoolManager) CreatePool(ctx context.Context, device string) error {
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

	// Configure error_if_no_space to fail immediately when full
	// rather than queueing I/O (which can cause hangs).
	if p.cfg.ErrorOnFull {
		args = append(args, "--errorwhenfull", "y")
	}

	if err := p.run.Run(ctx, "lvcreate", args...); err != nil {
		return fmt.Errorf("lvcreate thin-pool: %w", err)
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
