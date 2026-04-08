// Package poolguard monitors LVM thin pool utilization and protects the system
// by stopping workspace apps before the pool reaches 100% (which triggers DM
// error-IO mode and crashes all apps sharing the pool).
package poolguard

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"piccolod/internal/app"
	"piccolod/internal/events"
	"piccolod/internal/storage/lvm"
)

const (
	defaultInterval = 15 * time.Second
	defaultCooldown = 90 * time.Second
)

// PoolQuerier provides thin pool stats for the guard's polling loop.
type PoolQuerier interface {
	PoolStatus(ctx context.Context) (lvm.PoolStats, error)
}

// AppStopper is the subset of AppManager the guard needs.
type AppStopper interface {
	List(ctx context.Context) ([]*app.AppInstance, error)
	Stop(ctx context.Context, instanceID string) error
}

// Guard polls thin pool utilization and takes escalating protective action:
//   - At 90%: publishes a user-visible warning via the event bus.
//   - At 95%: stops all running workspace apps and publishes an urgent alert.
//
// Workspace apps are stopped first because they run user-directed unbounded
// workloads (fio, development). Service apps have bounded write patterns.
type Guard struct {
	pool     PoolQuerier
	apps     AppStopper
	bus      *events.Bus
	interval time.Duration
	cooldown time.Duration

	mu             sync.Mutex
	lastActionTime time.Time
	lastLevel      int // highest threshold seen; reset on recovery
	cancel         context.CancelFunc
}

// New creates a pool guard. Pass nil for pool or apps to create a no-op guard
// (e.g., when storage is not yet initialized).
func New(pool PoolQuerier, apps AppStopper, bus *events.Bus) *Guard {
	return &Guard{
		pool:     pool,
		apps:     apps,
		bus:      bus,
		interval: defaultInterval,
		cooldown: defaultCooldown,
	}
}

// Start begins the polling loop. Implements supervisor.Component via NewComponent.
func (g *Guard) Start(ctx context.Context) error {
	if g.pool == nil || g.apps == nil {
		return nil
	}
	pollCtx, cancel := context.WithCancel(ctx)
	g.cancel = cancel
	go g.loop(pollCtx)
	return nil
}

// Stop terminates the polling loop.
func (g *Guard) Stop(_ context.Context) error {
	if g.cancel != nil {
		g.cancel()
	}
	return nil
}

func (g *Guard) loop(ctx context.Context) {
	ticker := time.NewTicker(g.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.check(ctx)
		}
	}
}

func (g *Guard) check(ctx context.Context) {
	stats, err := g.pool.PoolStatus(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return // shutting down
		}
		log.Printf("WARN: pool-guard: failed to query pool status: %v", err)
		return
	}

	level := stats.ThresholdLevel()

	g.mu.Lock()
	prev := g.lastLevel
	if level < prev {
		// Pool recovered — reset so we can re-alert on next rise.
		g.lastLevel = level
		g.mu.Unlock()
		if prev >= lvm.ThresholdCritical {
			log.Printf("INFO: pool-guard: pool recovered to %.1f%% (threshold %d%% → %d%%)", stats.DataPercent, prev, level)
		}
		return
	}
	g.lastLevel = level
	g.mu.Unlock()

	// Alerts fire once per upward threshold crossing (dedup via level > prev).
	// Protective action (stopping workspaces) re-fires on every tick above
	// urgent threshold when cooldown allows — ensures the guard re-arms if
	// a user restarts a workspace while the pool is still critically full.
	switch {
	case level >= lvm.ThresholdUrgent:
		if level > prev {
			g.handleCritical(stats) // also publish the 90% warning on the way up
		}
		g.handleUrgent(ctx, stats)
	case level >= lvm.ThresholdCritical && level > prev:
		g.handleCritical(stats)
	}
}

func (g *Guard) handleCritical(stats lvm.PoolStats) {
	log.Printf("WARN: pool-guard: thin pool at %.1f%%, publishing user warning", stats.DataPercent)
	if g.bus != nil {
		g.bus.Publish(events.Event{
			Topic: events.TopicStorageAlert,
			Payload: events.StorageAlertEvent{
				Severity:    events.AlertSeverityCritical,
				Message:     "Storage is critically low. Consider cleaning up unused apps or files to prevent service disruption.",
				DataPercent: stats.DataPercent,
				Action:      events.AlertActionNone,
			},
		})
	}
}

func (g *Guard) handleUrgent(ctx context.Context, stats lvm.PoolStats) {
	g.mu.Lock()
	if time.Since(g.lastActionTime) < g.cooldown {
		g.mu.Unlock()
		log.Printf("INFO: pool-guard: urgent threshold at %.1f%% but cooldown active, skipping", stats.DataPercent)
		return
	}
	g.lastActionTime = time.Now()
	g.mu.Unlock()

	log.Printf("WARN: pool-guard: thin pool at %.1f%%, stopping workspace apps", stats.DataPercent)
	stopped := g.stopWorkspaceApps(ctx)

	if g.bus != nil {
		msg := "Storage critically full. Workspace apps have been stopped to prevent data loss. Free space and restart manually."
		if len(stopped) == 0 {
			msg = "Storage critically full. No running workspace apps to stop — check service app usage."
		}
		g.bus.Publish(events.Event{
			Topic: events.TopicStorageAlert,
			Payload: events.StorageAlertEvent{
				Severity:     events.AlertSeverityUrgent,
				Message:      msg,
				DataPercent:  stats.DataPercent,
				Action:       events.AlertActionWorkspacesStopped,
				AffectedApps: stopped,
			},
		})
	}
}

func (g *Guard) stopWorkspaceApps(ctx context.Context) []string {
	apps, err := g.apps.List(ctx)
	if err != nil {
		log.Printf("ERROR: pool-guard: failed to list apps: %v", err)
		return nil
	}

	var stopped []string
	for _, inst := range apps {
		if inst.Mode() != app.ModeWorkspace {
			continue
		}
		if inst.Status != app.StatusRunning && inst.Status != app.StatusStarting {
			continue
		}

		log.Printf("WARN: pool-guard: stopping workspace %s (pool >= %d%%)", inst.InstanceID, lvm.ThresholdUrgent)
		if err := g.apps.Stop(ctx, inst.InstanceID); err != nil {
			log.Printf("ERROR: pool-guard: failed to stop %s: %v", inst.InstanceID, err)
			continue
		}
		stopped = append(stopped, inst.InstanceID)
	}

	if len(stopped) == 0 {
		log.Printf("INFO: pool-guard: no running workspace apps to stop")
	} else {
		log.Printf("INFO: pool-guard: stopped %d workspace app(s): %v", len(stopped), stopped)
	}
	return stopped
}

// SetIntervalsForTest overrides polling and cooldown intervals.
// Must be called before Start — not safe for concurrent use.
func (g *Guard) SetIntervalsForTest(interval, cooldown time.Duration) {
	g.interval = interval
	g.cooldown = cooldown
}

// CheckNow runs one poll cycle synchronously. Exposed for testing.
func (g *Guard) CheckNow(ctx context.Context) {
	g.check(ctx)
}

// String implements fmt.Stringer for diagnostic logging.
func (g *Guard) String() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return fmt.Sprintf("pool-guard(lastLevel=%d, lastAction=%s)", g.lastLevel, g.lastActionTime.Format(time.RFC3339))
}
