package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"sort"
	"time"

	"piccolod/internal/api"
	"piccolod/internal/container"
	"piccolod/internal/resources"
	"piccolod/internal/resources/pressure"
)

const (
	memoryReliefThreshold = 10.0 // PSI some avg10, percent
	memoryReliefCooldown  = time.Minute
	memoryReliefSampleGap = 45 * time.Second
	memoryReliefMinStep   = int64(16 * 1024 * 1024)
	memoryReliefMaxProbes = 2
)

type memoryReliefState struct {
	identity        string
	cgroupID        uint64
	lastSample      time.Time
	lastHighEvents  uint64
	pressured       int
	healthy         int
	probes          int
	lastGrant       time.Time
	beforePressure  float64
	awaitFeedback   bool
	holdReason      string
	pendingHigh     int64
	pendingPressure float64
}

// RespondToMemoryPressure implements pressure.MemoryPressureResponder. This is
// a grant-time controller: it cannot reserve RAM against future workload growth.
// Lifecycle admission precedes the slice lock, matching manifest/uninstall work.
// Busy owners, shutdown, unknown telemetry, or task pressure defer the response.
func (m *AppManager) RespondToMemoryPressure(ctx context.Context, samples []pressure.MemorySample, host pressure.HostMemorySample) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pressure.DefaultAdmission.Check(ctx, pressure.WorkLifecycle); err != nil {
		return
	}
	ctx, release, ok := m.tryAcquireLifecycle(ctx)
	if !ok {
		return
	}
	defer release()
	if !sliceReconcileMu.TryLock() {
		return
	}
	defer sliceReconcileMu.Unlock()
	state, err := m.ensureStateManager()
	if err != nil || ctx.Err() != nil {
		return
	}
	apps := m.snapshotApps(false)
	for _, app := range apps {
		app.Status = m.getObservedStatus(app.InstanceID)
		// An interrupted transition can outlive lifecycle ownership. Its
		// definition/runtime generation is not a safe adjustment target.
		record, err := state.LoadTransitionRecord(app.InstanceID)
		app.TransitionActive = (err == nil && record != nil && record.Phase != TransitionPhaseCommitted) ||
			(err != nil && !errors.Is(err, os.ErrNotExist))
	}
	m.respondToMemoryPressureLocked(ctx, apps, samples, host, time.Now(), memoryReliefIO{
		now: time.Now,
		policy: func(app *AppInstance) (container.SliceResourcePolicy, error) {
			p, err := m.computeSliceResourcePolicy(app, 1) // only bounded apps enter
			if err != nil {
				return p, err
			}
			return p.RestoreAdaptiveMemoryHigh()
		},
		sample: pressure.ReadMemorySample,
		apply: func(ctx context.Context, p container.SliceResourcePolicy) error {
			return p.ApplyContext(ctx)
		},
	})
}

// The narrow I/O seam allows fixture tests to exercise this complete control
// loop without changing host cgroups or starting real systemctl processes.
type memoryReliefIO struct {
	now    func() time.Time
	policy func(*AppInstance) (container.SliceResourcePolicy, error)
	sample func(uint32) pressure.MemorySample
	apply  func(context.Context, container.SliceResourcePolicy) error
}

func (m *AppManager) respondToMemoryPressureLocked(ctx context.Context, apps []*AppInstance, samples []pressure.MemorySample, host pressure.HostMemorySample, now time.Time, io memoryReliefIO) {
	if m.memoryRelief == nil {
		m.memoryRelief = make(map[string]*memoryReliefState)
	}
	byUID := make(map[uint32]pressure.MemorySample, len(samples))
	for _, sample := range samples {
		if _, duplicate := byUID[sample.UID]; duplicate {
			sample.Valid = false
		}
		byUID[sample.UID] = sample
	}
	// Each round has one allowance, so simultaneous apps cannot each spend
	// the same available headroom. Keep half the surplus ungranted.
	allowance := memoryReliefAllowance(host)
	sort.Slice(apps, func(i, j int) bool { return apps[i].InstanceID < apps[j].InstanceID })
	seen := make(map[string]bool, len(apps))
	for _, app := range apps {
		if memoryReliefEligible(app) {
			seen[app.InstanceID] = true
		}
	}
	for _, app := range apps {
		if ctx.Err() != nil {
			break
		}
		if !memoryReliefEligible(app) {
			continue
		}
		p, err := io.policy(app)
		if err != nil || p.Validate() != nil || p.MemoryHighBytes <= 0 {
			// Uncertainty breaks consecutive evidence, not the episode's
			// probe cap or a file-first intent that might still be retried.
			if s := m.memoryRelief[app.InstanceID]; s != nil {
				s.lastSample = time.Time{}
				s.pressured, s.healthy = 0, 0
			}
			continue
		}
		sample := byUID[p.UID]
		baseline := p.MemoryHighBytes
		if p.BaselineMemoryHighBytes > 0 {
			baseline = p.BaselineMemoryHighBytes
		}
		identity := fmt.Sprintf("%d/%d/%d/%d", p.UID, baseline, p.MemoryMaxBytes, p.CPUWeight)
		s := m.memoryRelief[app.InstanceID]
		if s == nil {
			s = &memoryReliefState{}
			m.memoryRelief[app.InstanceID] = s
		}
		ready, feedback := s.observe(sample, p, identity, now)
		if s.pendingHigh > 0 {
			if p.MemoryHighBytes != s.pendingHigh {
				// No matching durable intent survived the failed application.
				s.pendingHigh = 0
			} else if validMemoryReliefSample(sample, p, now) && s.cgroupID == sample.CgroupID {
				// Reconciliation may have completed a previously failed live
				// application. Count it once when its effect is observable.
				s.recordGrant(io.now(), s.pendingPressure)
				ready = false
				log.Printf("INFO: memory relief %s: pending MemoryHigh %d confirmed live (probe %d/%d)",
					app.InstanceID, p.MemoryHighBytes, s.probes, memoryReliefMaxProbes)
			}
		}
		if feedback != "" {
			log.Printf("INFO: memory relief %s feedback: %s (some avg10=%.2f%%)", app.InstanceID, feedback, sample.SomeAvg10)
		}
		if !ready {
			continue
		}
		if s.probes >= memoryReliefMaxProbes {
			s.hold(app.InstanceID, "two adjustments did not resolve this pressure episode")
			continue
		}
		step := max(p.MemoryHighBytes/4, memoryReliefMinStep)
		step = min(step, p.MemoryMaxBytes-p.MemoryHighBytes)
		if step <= 0 {
			s.hold(app.InstanceID, "the unchanged memory hard ceiling has been reached")
			continue
		}
		if step > allowance {
			s.hold(app.InstanceID, "insufficient host memory headroom for a controlled increase")
			continue
		}
		// Recheck identity and effective limits immediately before changing
		// them: the poll snapshot may precede another owner or cgroup teardown.
		live := io.sample(p.UID)
		if !validMemoryReliefSample(live, p, io.now()) || live.CgroupID != sample.CgroupID ||
			live.HighEvents < sample.HighEvents || live.CurrentBytes < live.HighBytes*9/10 ||
			live.SomeAvg10 < memoryReliefThreshold {
			s.pressured = 0
			continue
		}
		next := p
		next.BaselineMemoryHighBytes = baseline
		next.MemoryHighBytes += step
		// File-first application can leave an intent for reconciliation to
		// retry even on live failure. Debit attempts conservatively this round.
		allowance -= step
		s.pendingHigh, s.pendingPressure = next.MemoryHighBytes, sample.SomeAvg10
		if err := io.apply(ctx, next); err != nil {
			s.hold(app.InstanceID, "soft-limit application failed; ordinary reconciliation will retry")
			log.Printf("WARN: memory relief apply %s: %v", app.InstanceID, err)
			continue
		}
		s.recordGrant(io.now(), sample.SomeAvg10)
		log.Printf("INFO: memory relief %s: MemoryHigh %d -> %d, MemoryMax remains %d (probe %d/%d)",
			app.InstanceID, p.MemoryHighBytes, next.MemoryHighBytes, p.MemoryMaxBytes, s.probes, memoryReliefMaxProbes)
	}
	for id := range m.memoryRelief {
		if !seen[id] {
			delete(m.memoryRelief, id)
		}
	}
}

func (s *memoryReliefState) recordGrant(now time.Time, beforePressure float64) {
	s.probes++
	s.lastGrant = now
	s.beforePressure = beforePressure
	s.awaitFeedback = true
	s.pressured = 0
	s.holdReason = ""
	s.pendingHigh = 0
}

func memoryReliefEligible(app *AppInstance) bool {
	return app != nil && app.Enabled && app.Status == StatusRunning && !app.TransitionActive &&
		app.Definition != nil && app.Definition.Resources != nil && app.Definition.Resources.Memory != nil &&
		(app.Definition.Resources.Memory.Profile == api.ProfileBounded || app.Definition.Resources.Memory.Profile == "")
}

func memoryReliefAllowance(host pressure.HostMemorySample) int64 {
	if !host.Valid || host.TotalBytes <= 0 || host.AvailableBytes < 0 || host.AvailableBytes > host.TotalBytes {
		return 0
	}
	reserve := max(host.TotalBytes/5, resources.SliceCeilingHeadroom)
	return max(int64(0), (host.AvailableBytes-reserve)/2)
}

func validMemoryReliefSample(sample pressure.MemorySample, p container.SliceResourcePolicy, now time.Time) bool {
	page := int64(os.Getpagesize())
	return sample.Valid && sample.UID == p.UID && sample.CgroupID != 0 && !sample.At.IsZero() &&
		!sample.At.After(now.Add(5*time.Second)) && now.Sub(sample.At) <= memoryReliefSampleGap &&
		sample.CurrentBytes >= 0 && sample.HighBytes > 0 && sample.MaxBytes > 0 &&
		sample.HighBytes == p.MemoryHighBytes/page*page && sample.MaxBytes == p.MemoryMaxBytes/page*page &&
		!math.IsNaN(sample.SomeAvg10) && !math.IsInf(sample.SomeAvg10, 0) && sample.SomeAvg10 >= 0 && sample.SomeAvg10 <= 100 &&
		!math.IsNaN(sample.FullAvg10) && !math.IsInf(sample.FullAvg10, 0) && sample.FullAvg10 >= 0 && sample.FullAvg10 <= 100
}

func (s *memoryReliefState) observe(sample pressure.MemorySample, p container.SliceResourcePolicy, identity string, now time.Time) (bool, string) {
	if !validMemoryReliefSample(sample, p, now) {
		s.lastSample = time.Time{}
		s.pressured, s.healthy = 0, 0
		return false, ""
	}
	if s.identity != identity || s.cgroupID != sample.CgroupID || (!s.lastSample.IsZero() && sample.HighEvents < s.lastHighEvents) {
		*s = memoryReliefState{identity: identity, cgroupID: sample.CgroupID}
	}
	if !s.lastSample.IsZero() && !sample.At.After(s.lastSample) {
		return false, "" // a replay must not supply another pressure observation
	}
	haveBaseline := !s.lastSample.IsZero() && sample.At.Sub(s.lastSample) <= memoryReliefSampleGap
	throttled := haveBaseline && sample.HighEvents > s.lastHighEvents
	s.lastSample, s.lastHighEvents = sample.At, sample.HighEvents
	if !haveBaseline {
		s.pressured, s.healthy = 0, 0
		return false, ""
	}
	feedback := ""
	if s.awaitFeedback && now.Sub(s.lastGrant) >= memoryReliefCooldown {
		feedback = "pressure did not materially improve"
		if sample.SomeAvg10 < memoryReliefThreshold || sample.SomeAvg10 <= s.beforePressure*0.8 {
			feedback = "pressure improved"
		}
		s.awaitFeedback = false
	}
	if sample.SomeAvg10 < memoryReliefThreshold {
		s.healthy++
		s.pressured = 0
		if s.healthy >= 2 {
			s.probes = 0
			s.holdReason = ""
			s.awaitFeedback = false
		}
		return false, feedback
	}
	s.healthy = 0
	if throttled && sample.CurrentBytes >= sample.HighBytes*9/10 {
		s.pressured++
	} else {
		s.pressured = 0
	}
	return s.pressured >= 2 && (s.lastGrant.IsZero() || now.Sub(s.lastGrant) >= memoryReliefCooldown), feedback
}

func (s *memoryReliefState) hold(app, reason string) {
	if s.holdReason != reason {
		log.Printf("WARN: memory relief held for %s: %s", app, reason)
		s.holdReason = reason
	}
}
