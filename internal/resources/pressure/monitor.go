// Package pressure implements the resource-pressure monitor component.
// See plan D-7, D-15 in piccolod/.claude/plans/resource-stewardship.md.
//
// The monitor polls per-app-user systemd slices for cgroup v2 Pressure
// Stall Information (PSI) and memory.events counters, debounces
// observations per (app, resource, severity) tuple, and emits
// TopicResourcePressure events via the event bus.
package pressure

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"piccolod/internal/events"
)

const (
	// pollInterval is how often the monitor samples PSI + events counters.
	pollInterval = 30 * time.Second

	// Emission debounce: one event per (app, resource, severity) within
	// this window. OOM-kill increments bypass sustained-pressure debounce
	// per D-7 (they're a strong-enough signal to notify immediately).
	emissionDebounce = 60 * time.Second

	// Reset window: after this much "below threshold" time, severity state
	// resets so the next excursion can re-notify at the appropriate level.
	severityResetWindow = 2 * time.Minute

	// PSI thresholds per D-7. "some avgN" percentages from memory.pressure /
	// cpu.pressure. "some" = any task stalled; "full" = all tasks stalled.
	psiInfoThresholdPercent   = 10.0 // sustained 30s → info
	psiWarnThresholdPercent   = 20.0 // sustained 60s → warn
	psiUrgentThresholdPercent = 40.0 // sustained → urgent

	infoSustainedSec   = 30
	warnSustainedSec   = 60
	urgentSustainedSec = 60
)

// AppLister is the interface the monitor uses to enumerate installed apps.
// Minimized to what the monitor needs so piccolod's AppManager can
// satisfy it without package-cycle gymnastics.
type AppLister interface {
	ListAppUIDs() []uint32
}

// Monitor is the per-app-slice pressure-attribution component.
type Monitor struct {
	bus    *events.Bus
	lister AppLister

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}

	// Per-slice state (keyed by UID). Tracks last-observed pressure levels
	// and last-emitted severity timestamps for debouncing.
	state map[uint32]*sliceState
}

type sliceState struct {
	// Last-observed PSI samples (for sustained-pressure detection).
	memSomeHistory []psiSample
	cpuSomeHistory []psiSample

	// Last OOM-kill counter seen; used to detect increments.
	lastOOMKill int64

	// Debounce: last emission time per (resource, severity).
	lastEmission map[string]time.Time

	// Most recent severity level per resource ("", info, warn, urgent) —
	// used to reset after the severity-reset window below threshold.
	lastSeverity map[string]string

	// Last time resource was observed below threshold (for reset detection).
	lastBelowThreshold map[string]time.Time
}

type psiSample struct {
	at      time.Time
	percent float64
}

// New returns a Monitor. Pass nil for either dep to produce a no-op monitor
// (Start returns without launching the goroutine).
func New(bus *events.Bus, lister AppLister) *Monitor {
	return &Monitor{
		bus:    bus,
		lister: lister,
		state:  make(map[uint32]*sliceState),
	}
}

// Start launches the polling loop. Implements supervisor.Component via NewComponent.
func (m *Monitor) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cancel != nil {
		return nil // already running
	}
	if m.bus == nil || m.lister == nil {
		return nil
	}
	pollCtx, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	m.done = make(chan struct{})
	go m.loop(pollCtx)
	log.Printf("INFO: resource pressure monitor started (interval=%s)", pollInterval)
	return nil
}

// Stop terminates the polling loop.
func (m *Monitor) Stop(ctx context.Context) error {
	m.mu.Lock()
	cancel := m.cancel
	done := m.done
	m.cancel = nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			log.Printf("WARN: resource pressure monitor stop timed out")
		}
	}
	return nil
}

func (m *Monitor) loop(ctx context.Context) {
	defer close(m.done)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	m.poll(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.poll(ctx)
		}
	}
}

func (m *Monitor) poll(ctx context.Context) {
	uids := m.lister.ListAppUIDs()

	// Release-and-reacquire style: brief lock for state access, release
	// before I/O so a slow cgroup read doesn't block other operations.
	m.mu.Lock()
	seen := make(map[uint32]bool, len(uids))
	for _, uid := range uids {
		seen[uid] = true
		if _, ok := m.state[uid]; !ok {
			m.state[uid] = &sliceState{
				lastEmission:       make(map[string]time.Time),
				lastSeverity:       make(map[string]string),
				lastBelowThreshold: make(map[string]time.Time),
			}
		}
	}
	// Garbage-collect state for uninstalled apps.
	for uid := range m.state {
		if !seen[uid] {
			delete(m.state, uid)
		}
	}
	m.mu.Unlock()

	for _, uid := range uids {
		m.sampleSlice(uid)
	}
}

func (m *Monitor) sampleSlice(uid uint32) {
	sliceBase := fmt.Sprintf("/sys/fs/cgroup/user.slice/user-%d.slice", uid)
	memSome := readPSI(filepath.Join(sliceBase, "memory.pressure"), "some")
	cpuSome := readPSI(filepath.Join(sliceBase, "cpu.pressure"), "some")
	oomKill := readOOMKillCount(filepath.Join(sliceBase, "memory.events"))

	now := time.Now()
	m.mu.Lock()
	state := m.state[uid]
	if state == nil {
		m.mu.Unlock()
		return
	}

	// Memory pressure: append sample, prune samples older than window.
	if memSome >= 0 {
		state.memSomeHistory = appendTrim(state.memSomeHistory, psiSample{at: now, percent: memSome}, 5*time.Minute)
	}
	if cpuSome >= 0 {
		state.cpuSomeHistory = appendTrim(state.cpuSomeHistory, psiSample{at: now, percent: cpuSome}, 5*time.Minute)
	}

	// Evaluate sustained thresholds.
	memSeverity := evaluateSustained(state.memSomeHistory, now)
	cpuSeverity := evaluateSustained(state.cpuSomeHistory, now)

	// OOM-kill delta bypasses sustained-pressure debounce.
	oomDelta := oomKill - state.lastOOMKill
	if oomKill >= state.lastOOMKill {
		state.lastOOMKill = oomKill
	} else {
		// counter reset (slice recreated) — reset baseline.
		state.lastOOMKill = oomKill
		oomDelta = 0
	}

	// Snapshot emission metadata we need with the lock, release, then emit.
	memShouldEmit, memReason := m.shouldEmitLocked(state, events.PressureResourceMemory, memSeverity, now)
	cpuShouldEmit, cpuReason := m.shouldEmitLocked(state, events.PressureResourceCPU, cpuSeverity, now)
	m.mu.Unlock()

	if memShouldEmit {
		m.emit(events.ResourcePressureEvent{
			Resource:      events.PressureResourceMemory,
			Severity:      memSeverity,
			AppInstanceID: fmt.Sprintf("uid:%d", uid),
			SliceMetric:   memSome,
			Message:       fmt.Sprintf("sustained memory pressure on user-%d.slice (%s)", uid, memReason),
		})
	}
	if cpuShouldEmit {
		m.emit(events.ResourcePressureEvent{
			Resource:      events.PressureResourceCPU,
			Severity:      cpuSeverity,
			AppInstanceID: fmt.Sprintf("uid:%d", uid),
			SliceMetric:   cpuSome,
			Message:       fmt.Sprintf("sustained cpu pressure on user-%d.slice (%s)", uid, cpuReason),
		})
	}

	if oomDelta > 0 {
		m.emit(events.ResourcePressureEvent{
			Resource:      events.PressureResourceMemory,
			Severity:      events.PressureSeverityWarn,
			AppInstanceID: fmt.Sprintf("uid:%d", uid),
			OOMKillCount:  oomDelta,
			Message:       fmt.Sprintf("%d OOM kill(s) in user-%d.slice since last observation", oomDelta, uid),
		})
	}
}

// shouldEmitLocked decides whether a pressure event should fire for the
// given (resource, severity) tuple. Must be called with m.mu held.
// Returns (true, reason-string) when the emission is due.
func (m *Monitor) shouldEmitLocked(state *sliceState, resource, severity string, now time.Time) (bool, string) {
	if severity == "" {
		// Below threshold. Record observation; if we've been below for
		// long enough, reset the tracked severity so future excursions
		// re-notify at the right level.
		if _, tracked := state.lastSeverity[resource]; tracked {
			if _, ok := state.lastBelowThreshold[resource]; !ok {
				state.lastBelowThreshold[resource] = now
			}
			if now.Sub(state.lastBelowThreshold[resource]) >= severityResetWindow {
				delete(state.lastSeverity, resource)
				delete(state.lastBelowThreshold, resource)
			}
		}
		return false, ""
	}
	// Above threshold: clear the below-clock.
	delete(state.lastBelowThreshold, resource)

	// Debounce: at most one emission per (resource, severity) per window.
	key := resource + ":" + severity
	if last, ok := state.lastEmission[key]; ok && now.Sub(last) < emissionDebounce {
		return false, ""
	}

	prevSeverity := state.lastSeverity[resource]
	state.lastSeverity[resource] = severity
	state.lastEmission[key] = now

	if prevSeverity == "" {
		return true, "first-excursion"
	}
	if severityRank(severity) > severityRank(prevSeverity) {
		return true, "escalated-from-" + prevSeverity
	}
	// Same or lower severity → only emit if debounce window expired (which
	// we already checked above).
	return true, "sustained"
}

func (m *Monitor) emit(ev events.ResourcePressureEvent) {
	if m.bus == nil {
		return
	}
	m.bus.Publish(events.Event{
		Topic:   events.TopicResourcePressure,
		Payload: ev,
	})
}

// appendTrim appends sample and drops samples older than cutoff.
func appendTrim(hist []psiSample, s psiSample, cutoff time.Duration) []psiSample {
	hist = append(hist, s)
	oldest := s.at.Add(-cutoff)
	// Drop leading samples that are before the cutoff.
	i := 0
	for i < len(hist) && hist[i].at.Before(oldest) {
		i++
	}
	if i > 0 {
		hist = hist[i:]
	}
	return hist
}

// evaluateSustained returns the severity name if recent samples sustain
// their respective threshold, or "" if below all thresholds.
func evaluateSustained(hist []psiSample, now time.Time) string {
	// Urgent: require urgent threshold sustained urgentSustainedSec.
	if sustained(hist, psiUrgentThresholdPercent, time.Duration(urgentSustainedSec)*time.Second, now) {
		return events.PressureSeverityUrgent
	}
	if sustained(hist, psiWarnThresholdPercent, time.Duration(warnSustainedSec)*time.Second, now) {
		return events.PressureSeverityWarn
	}
	if sustained(hist, psiInfoThresholdPercent, time.Duration(infoSustainedSec)*time.Second, now) {
		return events.PressureSeverityInfo
	}
	return ""
}

// sustained reports whether every sample in the last `window` interval is
// ≥ threshold. An empty window or no-samples case returns false.
func sustained(hist []psiSample, threshold float64, window time.Duration, now time.Time) bool {
	if len(hist) == 0 {
		return false
	}
	cutoff := now.Add(-window)
	// Walk back: need at least one sample at or before cutoff, and every
	// sample in the window ≥ threshold.
	haveEarly := false
	for _, s := range hist {
		if s.at.Before(cutoff) {
			haveEarly = true
			continue
		}
		if s.percent < threshold {
			return false
		}
	}
	return haveEarly
}

func severityRank(s string) int {
	switch s {
	case events.PressureSeverityInfo:
		return 1
	case events.PressureSeverityWarn:
		return 2
	case events.PressureSeverityUrgent:
		return 3
	}
	return 0
}

// readPSI parses a cgroup v2 pressure file and returns the "some" or "full"
// avg10 percentage. Returns -1 on error (file missing, unparseable).
func readPSI(path, kind string) float64 {
	f, err := os.Open(path)
	if err != nil {
		return -1
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if fields[0] != kind {
			continue
		}
		// fields: kind avg10=X.XX avg60=X.XX avg300=X.XX total=N
		for _, f := range fields[1:] {
			if strings.HasPrefix(f, "avg60=") {
				v, err := strconv.ParseFloat(strings.TrimPrefix(f, "avg60="), 64)
				if err == nil {
					return v
				}
			}
		}
	}
	return -1
}

// readOOMKillCount reads the `oom_kill` counter from a cgroup memory.events
// file. Returns 0 if absent/unparseable (safe default: no delta detected).
func readOOMKillCount(path string) int64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if fields[0] == "oom_kill" {
			v, err := strconv.ParseInt(fields[1], 10, 64)
			if err == nil {
				return v
			}
		}
	}
	return 0
}
