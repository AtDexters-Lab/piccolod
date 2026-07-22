package pressure

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"piccolod/internal/events"
	"piccolod/internal/health"
)

const (
	TaskGuardPollInterval = 5 * time.Second
	warningSustain        = 60 * time.Second
	maxCensusProcesses    = 64
)

type TaskPressureState string

const (
	TaskPressureNormal      TaskPressureState = "normal"
	TaskPressureWarning     TaskPressureState = "warning"
	TaskPressureCritical    TaskPressureState = "critical"
	TaskPressureUnavailable TaskPressureState = "unavailable"
)

const (
	ReasonNormal             = "normal"
	ReasonHighWater          = "high_water"
	ReasonSustainedHighWater = "sustained_high_water"
	ReasonMaxEvent           = "max_event"
	ReasonMonitorUnavailable = "monitor_unavailable"
)

type TaskSnapshot struct {
	State        TaskPressureState `json:"state"`
	ReasonCode   string            `json:"reason_code"`
	ActionTaken  string            `json:"action_taken,omitempty"`
	Current      int64             `json:"task_current,omitempty"`
	Limit        int64             `json:"task_limit,omitempty"`
	CurrentKnown bool              `json:"-"`
	LimitKnown   bool              `json:"-"`
	MaxEvents    uint64            `json:"max_events,omitempty"`
	SampledAt    time.Time         `json:"sampled_at"`
}

// AllowsAutomaticRecovery reports whether recovery owners have measured
// headroom below the Warning threshold. A malformed pids.events file degrades
// health and disables max-event detection, but it does not invalidate a known
// finite current/limit sample. Other monitor-unavailable states remain
// fail-closed because they cannot prove headroom.
func (s TaskSnapshot) AllowsAutomaticRecovery() bool {
	if s.State == TaskPressureNormal {
		return true
	}
	return s.State == TaskPressureUnavailable &&
		s.ReasonCode == ReasonMonitorUnavailable &&
		s.CurrentKnown && s.LimitKnown &&
		s.Current >= 0 && s.Limit > 0 &&
		s.Current <= (s.Limit-1)/2
}

type TaskProcess struct {
	PID     int    `json:"pid"`
	PPID    int    `json:"ppid"`
	Comm    string `json:"comm"`
	State   string `json:"state"`
	Threads int    `json:"threads"`
}

type TaskCensus struct {
	Snapshot       TaskSnapshot   `json:"snapshot"`
	Goroutines     int            `json:"goroutines"`
	Processes      []TaskProcess  `json:"top_processes,omitempty"`
	ByComm         map[string]int `json:"processes_by_comm,omitempty"`
	ByState        map[string]int `json:"processes_by_state,omitempty"`
	ThreadsByComm  map[string]int `json:"threads_by_comm,omitempty"`
	SessionCount   int            `json:"session_count,omitempty"`
	LifecycleOwner string         `json:"lifecycle_owner,omitempty"`
}

type TaskGuardConfig struct {
	CgroupRoot string
	ProcRoot   string
	Interval   time.Duration
	Now        func() time.Time
	Disabled   bool
	Admission  *AdmissionGate
	Health     *health.Tracker
	Bus        *events.Bus

	// CommitCritical must synchronously commit the exact snapshot to the
	// process-fatal first-wins owner without blocking. Production supplies the
	// cmd-owned atomic latch plus its capacity-one request channel.
	CommitCritical func(TaskSnapshot) bool
	Census         chan<- TaskCensus

	CloseDetached  func()
	OnNormal       func()
	SessionCount   func() int
	LifecycleOwner func() string
}

// TaskGuard samples only the Piccolod service cgroup. It owns one permanent
// goroutine regardless of the number of installed apps.
type TaskGuard struct {
	cfg TaskGuardConfig

	mu     sync.RWMutex
	cancel context.CancelFunc
	done   chan struct{}

	continuityRelay      restartContinuityRelay
	continuityGeneration uint64

	snapshot          TaskSnapshot
	pressureState     TaskPressureState
	cgroupPath        string
	maxBaseline       uint64
	baselineReady     bool
	highSamples       int
	belowSamples      int
	warningSince      time.Time
	criticalSignalled bool
	lastPublished     TaskSnapshot
}

func NewTaskGuard(cfg TaskGuardConfig) *TaskGuard {
	if cfg.CgroupRoot == "" {
		cfg.CgroupRoot = "/sys/fs/cgroup"
	}
	if cfg.ProcRoot == "" {
		cfg.ProcRoot = "/proc"
	}
	if cfg.Interval <= 0 {
		cfg.Interval = TaskGuardPollInterval
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Admission == nil {
		cfg.Admission = DefaultAdmission
	}
	guard := &TaskGuard{
		cfg:           cfg,
		pressureState: TaskPressureNormal,
		snapshot: TaskSnapshot{
			State:      TaskPressureUnavailable,
			ReasonCode: ReasonMonitorUnavailable,
		},
	}
	if cfg.Disabled {
		guard.snapshot = TaskSnapshot{
			State:      TaskPressureNormal,
			ReasonCode: ReasonNormal,
			SampledAt:  cfg.Now().UTC(),
		}
	}
	return guard
}

func (g *TaskGuard) Name() string { return "task-pressure" }

func (g *TaskGuard) Start(ctx context.Context) error {
	if g.cfg.Disabled {
		g.SampleNow()
		return nil
	}
	g.mu.Lock()
	if g.cancel != nil {
		g.mu.Unlock()
		return nil
	}
	loopCtx, cancel := context.WithCancel(ctx)
	g.cancel = cancel
	g.done = make(chan struct{})
	done := g.done
	g.mu.Unlock()

	go func() {
		defer close(done)
		g.SampleNow()
		ticker := time.NewTicker(g.cfg.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-ticker.C:
				g.SampleNow()
			}
		}
	}()
	return nil
}

func (g *TaskGuard) Stop(ctx context.Context) error {
	g.mu.Lock()
	cancel := g.cancel
	done := g.done
	g.cancel = nil
	g.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *TaskGuard) Snapshot() TaskSnapshot {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.snapshot
}

// AttachRestartContinuity installs the provider-neutral restart-unlock
// capability. The latest pressure transition is replayed asynchronously, so
// construction order cannot lose Warning, Normal, or Critical intent.
func (g *TaskGuard) AttachRestartContinuity(capability RestartContinuityCapability) {
	// Serialize attachment with transition recording so it cannot observe and
	// replay the state immediately preceding a concurrently committed Critical.
	g.mu.RLock()
	g.continuityRelay.attach(capability)
	g.mu.RUnlock()
}

// SampleNow is exported for deterministic fault tests; production calls it
// from the guard's single polling goroutine.
func (g *TaskGuard) SampleNow() TaskSnapshot {
	now := g.cfg.Now().UTC()
	if g.cfg.Disabled {
		snapshot := TaskSnapshot{State: TaskPressureNormal, ReasonCode: ReasonNormal, SampledAt: now}
		g.mu.Lock()
		g.pressureState = TaskPressureNormal
		g.snapshot = snapshot
		g.mu.Unlock()
		g.cfg.Admission.OpenPressure()
		g.report(snapshot, true)
		return snapshot
	}
	cgroupPath, err := resolveServiceCgroup(g.cfg.ProcRoot, g.cfg.CgroupRoot)
	if err != nil {
		return g.recordUnavailable(now, err)
	}
	current, err := readInt64File(filepath.Join(cgroupPath, "pids.current"))
	if err != nil {
		return g.recordUnavailable(now, fmt.Errorf("read pids.current: %w", err))
	}
	if current < 0 {
		return g.recordUnavailable(now, errors.New("pids.current is negative"))
	}
	limit, err := readFiniteLimit(filepath.Join(cgroupPath, "pids.max"))
	if err != nil {
		return g.recordUnavailable(now, err)
	}
	maxEvents, eventsOK := readPidsMaxEvents(filepath.Join(cgroupPath, "pids.events"))

	g.mu.Lock()
	pathChanged := cgroupPath != g.cgroupPath
	if pathChanged {
		g.cgroupPath = cgroupPath
		g.maxBaseline = maxEvents
		g.baselineReady = eventsOK
		g.highSamples = 0
		g.belowSamples = 0
		g.warningSince = time.Time{}
	}
	maxEventDelta := eventsOK && g.baselineReady && !pathChanged && maxEvents > g.maxBaseline
	if eventsOK {
		g.maxBaseline = maxEvents
		g.baselineReady = true
	}

	state := g.pressureState
	previousState := g.pressureState
	reason := ReasonNormal
	action := ""
	ratio := float64(current) / float64(limit)

	if g.criticalSignalled {
		state = TaskPressureCritical
		reason = g.snapshot.ReasonCode
		action = "piccolod_restart_requested"
	} else if maxEventDelta {
		state = TaskPressureCritical
		reason = ReasonMaxEvent
		action = "piccolod_restart_requested"
	} else if ratio >= 0.75 {
		state = TaskPressureCritical
		reason = ReasonHighWater
		action = "piccolod_restart_requested"
	} else {
		if ratio >= 0.50 {
			g.highSamples++
			g.belowSamples = 0
		} else if ratio < 0.40 {
			g.highSamples = 0
			g.belowSamples++
		} else {
			g.highSamples = 0
			g.belowSamples = 0
		}

		if state == TaskPressureWarning {
			if g.belowSamples >= 2 {
				state = TaskPressureNormal
				g.warningSince = time.Time{}
				g.cfg.Admission.OpenPressure()
			} else if ratio >= 0.50 && !g.warningSince.IsZero() && now.Sub(g.warningSince) >= warningSustain {
				state = TaskPressureCritical
				reason = ReasonSustainedHighWater
				action = "piccolod_restart_requested"
			} else {
				reason = ReasonHighWater
				action = "admission_fenced"
			}
		} else if g.highSamples >= 2 {
			state = TaskPressureWarning
			reason = ReasonHighWater
			action = "admission_fenced"
			g.warningSince = now
			g.cfg.Admission.Fence()
		}
	}

	if state == TaskPressureCritical {
		g.cfg.Admission.FenceCritical()
		g.criticalSignalled = true
	} else if state == TaskPressureNormal && (eventsOK || ratio < 0.50) {
		// A malformed pids.events file disables only max-event detection. A
		// known current/limit sample below Warning can therefore release an
		// earlier monitor-unavailable fence while health remains degraded.
		// Opening this latch cannot bypass startup or process-fatal fences.
		g.cfg.Admission.OpenPressure()
	}
	g.pressureState = state
	var continuityIntent RestartContinuityIntent
	if state != previousState && isRestartContinuityState(state) {
		g.continuityGeneration++
		continuityIntent = RestartContinuityIntent{
			State:      state,
			Generation: g.continuityGeneration,
		}
	}
	publishedState := state
	publishedReason := reason
	publishedAction := action
	if !eventsOK && state == TaskPressureNormal {
		publishedState = TaskPressureUnavailable
		publishedReason = ReasonMonitorUnavailable
		publishedAction = ""
	}
	snapshot := TaskSnapshot{
		State:        publishedState,
		ReasonCode:   publishedReason,
		ActionTaken:  publishedAction,
		Current:      current,
		Limit:        limit,
		CurrentKnown: true,
		LimitKnown:   true,
		MaxEvents:    maxEvents,
		SampledAt:    now,
	}
	g.snapshot = snapshot
	criticalTransition := previousState != TaskPressureCritical && state == TaskPressureCritical
	if criticalTransition {
		// Commit the exact snapshot to the process-fatal first-wins latch while
		// the transition is committed and before making Critical visible to the
		// asynchronous continuity capability.
		g.commitCriticalOwner(snapshot)
	}
	g.continuityRelay.publish(continuityIntent)
	g.mu.Unlock()

	if criticalTransition {
		g.collectCriticalCensus(snapshot, cgroupPath)
	}
	g.report(snapshot, eventsOK)
	if previousState != TaskPressureWarning && state == TaskPressureWarning && g.cfg.CloseDetached != nil {
		// Warning shedding must not stop the guard's only sampler. Production
		// removal is non-joining; the goroutine also contains an unexpected
		// blocking callback without delaying later Critical escalation.
		go g.cfg.CloseDetached()
	}
	if previousState == TaskPressureWarning && state == TaskPressureNormal && g.cfg.OnNormal != nil {
		g.cfg.OnNormal()
	}
	return snapshot
}

func isRestartContinuityState(state TaskPressureState) bool {
	switch state {
	case TaskPressureNormal, TaskPressureWarning, TaskPressureCritical:
		return true
	default:
		return false
	}
}

func (g *TaskGuard) recordUnavailable(now time.Time, cause error) TaskSnapshot {
	snapshot := TaskSnapshot{
		State:      TaskPressureUnavailable,
		ReasonCode: ReasonMonitorUnavailable,
		SampledAt:  now,
	}
	g.mu.Lock()
	if g.criticalSignalled {
		snapshot = g.snapshot
	} else {
		g.snapshot = snapshot
	}
	g.mu.Unlock()
	g.cfg.Admission.FenceUnavailable()
	if g.cfg.Health != nil {
		g.cfg.Health.Set("task-pressure", health.Status{
			Level:   health.LevelError,
			Message: "task pressure monitor unavailable",
			Details: map[string]interface{}{"reason_code": ReasonMonitorUnavailable, "error": cause.Error()},
		})
	}
	g.publish(snapshot)
	return snapshot
}

func (g *TaskGuard) report(snapshot TaskSnapshot, eventsOK bool) {
	if g.cfg.Health != nil {
		level := health.LevelOK
		message := "task pressure normal"
		switch snapshot.State {
		case TaskPressureWarning:
			level = health.LevelWarn
			message = "task pressure admission fenced"
		case TaskPressureCritical:
			level = health.LevelError
			message = "task pressure restart requested"
		}
		if !eventsOK && level < health.LevelError {
			level = health.LevelError
			message = "task max-event monitor unavailable"
		}
		g.cfg.Health.Set("task-pressure", health.Status{
			Level:   level,
			Message: message,
			Details: map[string]interface{}{
				"reason_code":  snapshot.ReasonCode,
				"task_current": snapshot.Current,
				"task_limit":   snapshot.Limit,
			},
		})
	}
	g.publish(snapshot)
}

func (g *TaskGuard) publish(snapshot TaskSnapshot) {
	if g.cfg.Bus == nil {
		return
	}
	g.mu.Lock()
	if samePublishedTaskSnapshot(g.lastPublished, snapshot) {
		g.mu.Unlock()
		return
	}
	g.lastPublished = snapshot
	g.mu.Unlock()
	ev := events.ResourcePressureEvent{
		Resource:    events.PressureResourceTasks,
		Severity:    events.PressureSeverityOK,
		ReasonCode:  snapshot.ReasonCode,
		ActionTaken: snapshot.ActionTaken,
	}
	if snapshot.CurrentKnown {
		v := snapshot.Current
		ev.TaskCurrent = &v
	}
	if snapshot.LimitKnown {
		v := snapshot.Limit
		ev.TaskLimit = &v
	}
	switch snapshot.State {
	case TaskPressureWarning, TaskPressureUnavailable:
		ev.Severity = events.PressureSeverityWarn
	case TaskPressureCritical:
		ev.Severity = events.PressureSeverityUrgent
	}
	g.cfg.Bus.Publish(events.Event{Topic: events.TopicResourcePressure, Payload: ev})
}

func samePublishedTaskSnapshot(a, b TaskSnapshot) bool {
	return a.State == b.State && a.ReasonCode == b.ReasonCode && a.ActionTaken == b.ActionTaken &&
		a.Current == b.Current && a.Limit == b.Limit && a.CurrentKnown == b.CurrentKnown && a.LimitKnown == b.LimitKnown
}

func (g *TaskGuard) commitCriticalOwner(snapshot TaskSnapshot) {
	// This bounded producer boundary arms the process owner's exit deadline. It
	// must precede every continuity callback, reporter, logger, and best-effort
	// census path because any of those can block under the exhaustion we are
	// escaping. A false return means another fatal source already owns exit.
	if g.cfg.CommitCritical != nil {
		g.cfg.CommitCritical(snapshot)
	}
}

func (g *TaskGuard) collectCriticalCensus(snapshot TaskSnapshot, cgroupPath string) {
	if g.cfg.Census == nil {
		return
	}
	census := collectTaskCensus(g.cfg.ProcRoot, cgroupPath, snapshot)
	if g.cfg.SessionCount != nil {
		census.SessionCount = g.cfg.SessionCount()
	}
	if g.cfg.LifecycleOwner != nil {
		census.LifecycleOwner = g.cfg.LifecycleOwner()
	}
	select {
	case g.cfg.Census <- census:
	default:
	}
}

func resolveServiceCgroup(procRoot, cgroupRoot string) (string, error) {
	data, err := os.ReadFile(filepath.Join(procRoot, "self", "cgroup"))
	if err != nil {
		return "", fmt.Errorf("read self cgroup: %w", err)
	}
	var unified string
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), ":", 3)
		if len(parts) == 3 && parts[0] == "0" && parts[1] == "" {
			unified = parts[2]
			break
		}
	}
	if unified == "" || !filepath.IsAbs(unified) {
		return "", errors.New("unified service cgroup not found")
	}
	root, err := filepath.Abs(cgroupRoot)
	if err != nil {
		return "", err
	}
	candidate := filepath.Join(root, strings.TrimPrefix(filepath.Clean(unified), string(filepath.Separator)))
	rel, err := filepath.Rel(root, candidate)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("service cgroup escapes cgroup root")
	}
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve cgroup root: %w", err)
	}
	candidateReal, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve service cgroup: %w", err)
	}
	rel, err = filepath.Rel(rootReal, candidateReal)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("resolved service cgroup escapes cgroup root")
	}
	return candidateReal, nil
}

func readInt64File(path string) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
}

func readFiniteLimit(path string) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read pids.max: %w", err)
	}
	raw := strings.TrimSpace(string(data))
	if raw == "max" {
		return 0, errors.New("pids.max is unlimited")
	}
	limit, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || limit <= 0 {
		return 0, fmt.Errorf("invalid pids.max %q", raw)
	}
	return limit, nil
}

func readPidsMaxEvents(path string) (uint64, bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 || fields[0] != "max" {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		return value, err == nil
	}
	return 0, false
}

func collectTaskCensus(procRoot, cgroupPath string, snapshot TaskSnapshot) TaskCensus {
	census := TaskCensus{
		Snapshot:      snapshot,
		Goroutines:    runtime.NumGoroutine(),
		ByComm:        make(map[string]int),
		ByState:       make(map[string]int),
		ThreadsByComm: make(map[string]int),
	}
	pids := make(map[int]struct{})
	_ = filepath.WalkDir(cgroupPath, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || !entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(filepath.Join(path, "cgroup.procs"))
		if err != nil {
			return nil
		}
		for _, field := range strings.Fields(string(data)) {
			pid, err := strconv.Atoi(field)
			if err == nil && pid > 0 {
				pids[pid] = struct{}{}
			}
		}
		return nil
	})
	for pid := range pids {
		process, ok := readTaskProcess(procRoot, pid)
		if !ok {
			continue
		}
		census.Processes = append(census.Processes, process)
		census.ByComm[process.Comm]++
		census.ByState[process.State]++
		census.ThreadsByComm[process.Comm] += process.Threads
	}
	sort.Slice(census.Processes, func(i, j int) bool {
		if census.Processes[i].Threads == census.Processes[j].Threads {
			return census.Processes[i].PID < census.Processes[j].PID
		}
		return census.Processes[i].Threads > census.Processes[j].Threads
	})
	if len(census.Processes) > maxCensusProcesses {
		census.Processes = census.Processes[:maxCensusProcesses]
	}
	return census
}

func readTaskProcess(procRoot string, pid int) (TaskProcess, bool) {
	data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "status"))
	if err != nil {
		return TaskProcess{}, false
	}
	p := TaskProcess{PID: pid}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch key {
		case "Name":
			p.Comm = value
		case "State":
			if fields := strings.Fields(value); len(fields) > 0 {
				p.State = fields[0]
			}
		case "PPid":
			p.PPID, _ = strconv.Atoi(value)
		case "Threads":
			p.Threads, _ = strconv.Atoi(value)
		}
	}
	if p.Comm == "" {
		p.Comm = "unknown"
	}
	return p, true
}
