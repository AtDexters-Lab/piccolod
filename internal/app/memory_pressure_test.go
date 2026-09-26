package app

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"testing"
	"time"

	"piccolod/internal/api"
	"piccolod/internal/container"
	"piccolod/internal/resources/pressure"
)

type memoryReliefFixture struct {
	m        *AppManager
	apps     []*AppInstance
	policy   map[string]container.SliceResourcePolicy
	live     map[uint32]pressure.MemorySample
	grants   []container.SliceResourcePolicy
	host     pressure.HostMemorySample
	applyErr error
	readErr  error
	recheck  func(pressure.MemorySample) pressure.MemorySample
	now      time.Time
}

func newMemoryReliefFixture(ids ...string) *memoryReliefFixture {
	f := &memoryReliefFixture{
		m: &AppManager{}, policy: make(map[string]container.SliceResourcePolicy),
		live: make(map[uint32]pressure.MemorySample),
		host: pressure.HostMemorySample{Valid: true, TotalBytes: 2_000_000_000, AvailableBytes: 700_000_000},
	}
	for i, id := range ids {
		f.apps = append(f.apps, &AppInstance{InstanceID: id, Enabled: true, Status: StatusRunning,
			Definition: &api.AppDefinition{Resources: &api.AppResources{Memory: &api.ResourceMemory{MinRequired: "64MB", Profile: api.ProfileBounded}}}})
		f.policy[id] = container.SliceResourcePolicy{UID: uint32(467 + i), MemoryHighBytes: 80_000_000, MemoryMaxBytes: 128_000_000, CPUWeight: 100}
	}
	return f
}

func (f *memoryReliefFixture) io() memoryReliefIO {
	return memoryReliefIO{
		now: func() time.Time { return f.now },
		policy: func(app *AppInstance) (container.SliceResourcePolicy, error) {
			return f.policy[app.InstanceID], f.readErr
		},
		sample: func(uid uint32) pressure.MemorySample {
			s := f.live[uid]
			if f.recheck != nil {
				return f.recheck(s)
			}
			return s
		},
		apply: func(ctx context.Context, p container.SliceResourcePolicy) error {
			if f.applyErr != nil {
				return f.applyErr
			}
			for id, previous := range f.policy {
				if previous.UID == p.UID {
					f.policy[id] = p
				}
			}
			f.grants = append(f.grants, p)
			return nil
		},
	}
}

func (f *memoryReliefFixture) samples(seconds int, some float64, events uint64) []pressure.MemorySample {
	page := int64(os.Getpagesize())
	at := time.Unix(1_000_000+int64(seconds), 0)
	f.now = at
	samples := make([]pressure.MemorySample, 0, len(f.policy))
	for _, app := range f.apps {
		p := f.policy[app.InstanceID]
		s := pressure.MemorySample{UID: p.UID, At: at, Valid: true, CgroupID: uint64(p.UID),
			HighBytes: p.MemoryHighBytes / page * page, MaxBytes: p.MemoryMaxBytes / page * page,
			CurrentBytes: p.MemoryHighBytes / page * page, HighEvents: events, SomeAvg10: some, FullAvg10: some}
		f.live[p.UID] = s
		samples = append(samples, s)
	}
	return samples
}

func (f *memoryReliefFixture) round(seconds int, some float64, events uint64) {
	samples := f.samples(seconds, some, events)
	f.m.respondToMemoryPressureLocked(context.Background(), f.apps, samples, f.host, samples[0].At, f.io())
}

func TestMemoryReliefLandingPressureRecovery(t *testing.T) {
	f := newMemoryReliefFixture("landing")
	wantDefinition, err := json.Marshal(f.apps[0].Definition)
	if err != nil {
		t.Fatal(err)
	}
	f.round(0, 100, 0)
	f.round(30, 100, 1)
	if len(f.grants) != 0 {
		t.Fatal("single throttle sample triggered relief")
	}
	f.round(60, 100, 2)
	if len(f.grants) != 1 || f.grants[0].MemoryHighBytes != 100_000_000 {
		t.Fatalf("first grant: %+v", f.grants)
	}
	f.round(90, 100, 3)
	if len(f.grants) != 1 {
		t.Fatal("cooldown was bypassed")
	}
	// A ~108 MB workload can remain fully stalled at 100 MB. Permit the
	// second bounded probe without requiring the first one to improve PSI.
	f.round(120, 100, 4)
	if len(f.grants) != 2 || f.grants[1].MemoryHighBytes != 125_000_000 {
		t.Fatalf("second grant: %+v", f.grants)
	}
	f.round(150, 0, 4)
	f.round(180, 0, 4)
	if s := f.m.memoryRelief["landing"]; s.probes != 0 || s.awaitFeedback {
		t.Fatalf("recovery did not reset episode: %+v", s)
	}
	for _, p := range f.grants {
		if p.MemoryMaxBytes != 128_000_000 || p.BaselineMemoryHighBytes != 80_000_000 {
			t.Fatalf("hard ceiling or baseline changed: %+v", p)
		}
	}
	gotDefinition, err := json.Marshal(f.apps[0].Definition)
	if err != nil || string(gotDefinition) != string(wantDefinition) {
		t.Fatal("controller changed developer declaration")
	}
}

func TestMemoryReliefStopsAfterTwoProbesAndRecovers(t *testing.T) {
	f := newMemoryReliefFixture("landing")
	for i := 0; i <= 8; i++ {
		f.round(i*30, 90, uint64(i))
	}
	if len(f.grants) != 2 {
		t.Fatalf("unbounded/no relief: got %d grants", len(f.grants))
	}
	f.round(270, 0, 8)
	if f.m.memoryRelief["landing"].probes != 2 {
		t.Fatal("one healthy sample reset episode")
	}
	f.round(300, 0, 8)
	f.round(330, 90, 9)
	f.round(360, 90, 10)
	if len(f.grants) != 3 || f.grants[2].MemoryHighBytes != 128_000_000 {
		t.Fatalf("bounded next episode: %+v", f.grants)
	}
	for i := 13; i <= 18; i++ {
		f.round(i*30, 90, uint64(i))
	}
	if len(f.grants) != 3 {
		t.Fatal("controller exceeded unchanged hard ceiling")
	}
}

func TestMemoryReliefRequiresLocalThrottlingAndCapacity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		some   float64
		events bool
		host   pressure.HostMemorySample
	}{
		{"PSI without high events", 90, false, pressure.HostMemorySample{Valid: true, TotalBytes: 2_000_000_000, AvailableBytes: 700_000_000}},
		{"usage and high events without PSI", 0, true, pressure.HostMemorySample{Valid: true, TotalBytes: 2_000_000_000, AvailableBytes: 700_000_000}},
		{"host reserve", 90, true, pressure.HostMemorySample{Valid: true, TotalBytes: 2_000_000_000, AvailableBytes: 400_000_000}},
		{"unknown host", 90, true, pressure.HostMemorySample{}},
		{"invalid available", 90, true, pressure.HostMemorySample{Valid: true, TotalBytes: 2_000_000_000, AvailableBytes: 3_000_000_000}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMemoryReliefFixture("landing")
			f.host = tc.host
			for i := 0; i <= 4; i++ {
				var count uint64
				if tc.events {
					count = uint64(i)
				}
				f.round(i*30, tc.some, count)
			}
			if len(f.grants) != 0 {
				t.Fatalf("unsafe grant: %+v", f.grants)
			}
		})
	}
}

func TestMemoryReliefSharesRoundHeadroom(t *testing.T) {
	f := newMemoryReliefFixture("alpha", "beta")
	f.host.AvailableBytes = 444_000_000 // allowance 22 MB, one 20 MB step
	f.round(0, 90, 0)
	f.round(30, 90, 1)
	f.round(60, 90, 2)
	if len(f.grants) != 1 {
		t.Fatalf("same headroom spent twice: %+v", f.grants)
	}
	if f.grants[0].MemoryHighBytes-80_000_000 > memoryReliefAllowance(f.host) {
		t.Fatal("round allowance exceeded")
	}
}

func TestMemoryReliefSlowApplyKeepsLaterAppRecheckFresh(t *testing.T) {
	f := newMemoryReliefFixture("alpha", "beta")
	f.round(0, 90, 0)
	f.round(30, 90, 1)
	samples := f.samples(60, 90, 2)
	io := f.io()
	apply := io.apply
	io.apply = func(ctx context.Context, p container.SliceResourcePolicy) error {
		if p.UID == f.policy["alpha"].UID {
			// A slow first write can consume most of the ten-second response
			// budget; beta's recheck is still fresh when its turn arrives.
			f.now = f.now.Add(6 * time.Second)
		}
		return apply(ctx, p)
	}
	f.recheck = func(s pressure.MemorySample) pressure.MemorySample {
		s.At = f.now
		return s
	}
	f.m.respondToMemoryPressureLocked(context.Background(), f.apps, samples, f.host, samples[0].At, io)
	if len(f.grants) != 2 {
		t.Fatalf("slow first apply blocked fresh later telemetry: grants=%+v", f.grants)
	}
	for _, app := range f.apps {
		if got := f.m.memoryRelief[app.InstanceID].lastGrant; !got.Equal(f.now) {
			t.Fatalf("%s cooldown starts at %v, want successful application time %v", app.InstanceID, got, f.now)
		}
	}
	// Even two more pressure observations must not permit a second probe
	// until a full cooldown has elapsed since the successful slow write.
	f.round(90, 90, 3)
	f.round(120, 90, 4)
	if len(f.grants) != 2 {
		t.Fatal("slow application shortened the cooldown")
	}
}

func TestMemoryReliefSkipsIneligibleApps(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*AppInstance)
	}{
		{"disabled", func(a *AppInstance) { a.Enabled = false }},
		{"starting", func(a *AppInstance) { a.Status = StatusStarting }},
		{"uninstalling", func(a *AppInstance) { a.Status = StatusUninstalling }},
		{"transition", func(a *AppInstance) { a.TransitionActive = true }},
		{"elastic", func(a *AppInstance) { a.Definition.Resources.Memory.Profile = api.ProfileElastic }},
		{"undeclared", func(a *AppInstance) { a.Definition.Resources = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMemoryReliefFixture("landing")
			tc.change(f.apps[0])
			for i := 0; i <= 4; i++ {
				f.round(i*30, 90, uint64(i))
			}
			if len(f.grants) != 0 || len(f.m.memoryRelief) != 0 {
				t.Fatal("ineligible app received/tracked relief")
			}
		})
	}
}

func TestMemoryReliefInvalidatesStaleEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*pressure.MemorySample)
	}{
		{"missing", func(s *pressure.MemorySample) { s.Valid = false }},
		{"gap", func(s *pressure.MemorySample) { s.At = s.At.Add(time.Minute) }},
		{"replacement", func(s *pressure.MemorySample) { s.CgroupID++ }},
		{"counter reset", func(s *pressure.MemorySample) { s.HighEvents = 0 }},
		{"foreign live limit", func(s *pressure.MemorySample) { s.HighBytes += int64(os.Getpagesize()) }},
		{"nonfinite pressure", func(s *pressure.MemorySample) { s.SomeAvg10 = math.NaN() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMemoryReliefFixture("landing")
			f.round(0, 90, 0)
			f.round(30, 90, 1)
			samples := f.samples(60, 90, 2)
			tc.change(&samples[0])
			f.live[samples[0].UID] = samples[0]
			f.m.respondToMemoryPressureLocked(context.Background(), f.apps, samples, f.host, samples[0].At, f.io())
			if len(f.grants) != 0 {
				t.Fatalf("stale evidence triggered relief: %+v", f.grants)
			}
		})
	}
}

func TestMemoryReliefRechecksAndDoesNotCountFailedWrites(t *testing.T) {
	for _, tc := range []struct {
		name      string
		configure func(*memoryReliefFixture)
	}{
		{"recreated before write", func(f *memoryReliefFixture) {
			f.recheck = func(s pressure.MemorySample) pressure.MemorySample { s.CgroupID++; return s }
		}},
		{"load ended before write", func(f *memoryReliefFixture) {
			f.recheck = func(s pressure.MemorySample) pressure.MemorySample { s.CurrentBytes = 0; return s }
		}},
		{"write failed", func(f *memoryReliefFixture) { f.applyErr = errors.New("systemctl unavailable") }},
		{"persistence unavailable", func(f *memoryReliefFixture) { f.readErr = errors.New("cannot read policy") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMemoryReliefFixture("landing")
			tc.configure(f)
			f.round(0, 90, 0)
			f.round(30, 90, 1)
			f.round(60, 90, 2)
			if len(f.grants) != 0 {
				t.Fatal("unsafe write")
			}
			if s := f.m.memoryRelief["landing"]; s != nil && (s.probes != 0 || s.awaitFeedback) {
				t.Fatalf("failed write counted as relief: %+v", s)
			}
		})
	}
}

func TestMemoryReliefAdmissionDoesNotWaitOrLeak(t *testing.T) {
	m := &AppManager{}
	release, ok := m.lifecycleGate.tryAcquire()
	if !ok {
		t.Fatal("initial admission")
	}
	m.RespondToMemoryPressure(context.Background(), nil, pressure.HostMemorySample{})
	release()
	sliceReconcileMu.Lock()
	m.RespondToMemoryPressure(context.Background(), nil, pressure.HostMemorySample{})
	sliceReconcileMu.Unlock()
	release, ok = m.lifecycleGate.tryAcquire()
	if !ok {
		t.Fatal("slice contention leaked lifecycle admission")
	}
	release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.RespondToMemoryPressure(ctx, nil, pressure.HostMemorySample{})
	if m.memoryRelief != nil {
		t.Fatal("blocked/canceled response performed work")
	}
}

func TestMemoryReliefReconciledFailedIntentCountsOneProbe(t *testing.T) {
	f := newMemoryReliefFixture("landing")
	f.round(0, 100, 0)
	f.round(30, 100, 1)
	io := f.io()
	io.apply = func(_ context.Context, p container.SliceResourcePolicy) error {
		// Production writes its generated intent before calling systemctl.
		f.policy["landing"] = p
		return errors.New("live set-property failed after persisting intent")
	}
	samples := f.samples(60, 100, 2)
	f.m.respondToMemoryPressureLocked(context.Background(), f.apps, samples, f.host, samples[0].At, io)
	s := f.m.memoryRelief["landing"]
	if s.probes != 0 || s.pendingHigh != 100_000_000 || s.awaitFeedback {
		t.Fatalf("failed live write reported success or lost intent: %+v", s)
	}
	// Before reconciliation, the actual cgroup still has the original high.
	samples = f.samples(90, 100, 3)
	samples[0].HighBytes = 80_000_000 / int64(os.Getpagesize()) * int64(os.Getpagesize())
	samples[0].CurrentBytes = samples[0].HighBytes
	f.live[samples[0].UID] = samples[0]
	f.m.respondToMemoryPressureLocked(context.Background(), f.apps, samples, f.host, samples[0].At, f.io())
	if s.probes != 0 || s.pendingHigh == 0 {
		t.Fatal("unapplied intent was counted or discarded")
	}
	// Ordinary reconciliation applies the saved high. Fresh telemetry confirms
	// that effect; it counts exactly once and starts a new cooldown.
	f.round(120, 100, 4)
	if s.probes != 1 || s.pendingHigh != 0 || !s.awaitFeedback || !s.lastGrant.Equal(time.Unix(1_000_120, 0)) {
		t.Fatalf("reconciled intent was not confirmed exactly once: %+v", s)
	}
	f.round(150, 100, 5)
	if len(f.grants) != 0 || s.probes != 1 {
		t.Fatal("pending confirmation bypassed cooldown or was counted twice")
	}
	f.round(180, 100, 6)
	for i := 7; i <= 11; i++ {
		f.round(i*30, 100, uint64(i))
	}
	if len(f.grants) != 1 || f.policy["landing"].MemoryHighBytes != 125_000_000 || s.probes != 2 {
		t.Fatalf("reconciliation bypassed the two-probe cap: grants=%+v state=%+v", f.grants, s)
	}
}

func TestMemoryReliefReadFailureAndCanceledRoundPreserveProbeCap(t *testing.T) {
	f := newMemoryReliefFixture("landing")
	for i := 0; i <= 4; i++ {
		f.round(i*30, 100, uint64(i))
	}
	if f.m.memoryRelief["landing"].probes != 2 {
		t.Fatal("fixture did not use two probes")
	}
	f.readErr = errors.New("temporary policy read failure")
	f.round(150, 100, 5)
	f.readErr = nil
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	samples := f.samples(180, 100, 6)
	f.m.respondToMemoryPressureLocked(ctx, f.apps, samples, f.host, samples[0].At, f.io())
	for i := 7; i <= 10; i++ {
		f.round(i*30, 100, uint64(i))
	}
	if len(f.grants) != 2 || f.m.memoryRelief["landing"].probes != 2 {
		t.Fatal("uncertain/canceled round discarded the probe cap")
	}
}

func TestMemoryReliefReplayCannotSupplyPressureOrRecoveryEvidence(t *testing.T) {
	t.Run("pressure", func(t *testing.T) {
		f := newMemoryReliefFixture("landing")
		f.round(0, 100, 0)
		f.round(30, 100, 1)
		samples := f.samples(30, 100, 2)
		for i := 0; i < 8; i++ {
			f.m.respondToMemoryPressureLocked(context.Background(), f.apps, samples, f.host, samples[0].At.Add(time.Duration(i)*time.Second), f.io())
		}
		// An older observation is also a replay even if its counter is larger.
		samples[0].At = samples[0].At.Add(-time.Second)
		f.m.respondToMemoryPressureLocked(context.Background(), f.apps, samples, f.host, time.Unix(1_000_040, 0), f.io())
		if len(f.grants) != 0 || f.m.memoryRelief["landing"].pressured != 1 {
			t.Fatal("replayed pressure supplied the second observation")
		}
		f.round(60, 100, 2)
		if len(f.grants) != 1 {
			t.Fatal("fresh pressure could not complete the observation window")
		}
	})
	t.Run("recovery", func(t *testing.T) {
		f := newMemoryReliefFixture("landing")
		for i := 0; i <= 4; i++ {
			f.round(i*30, 100, uint64(i))
		}
		f.round(150, 0, 4)
		samples := f.samples(150, 0, 4)
		for i := 0; i < 8; i++ {
			f.m.respondToMemoryPressureLocked(context.Background(), f.apps, samples, f.host, samples[0].At.Add(time.Duration(i)*time.Second), f.io())
		}
		if f.m.memoryRelief["landing"].probes != 2 || f.m.memoryRelief["landing"].healthy != 1 {
			t.Fatal("replayed healthy telemetry reopened the pressure episode")
		}
		f.round(180, 0, 4)
		if f.m.memoryRelief["landing"].probes != 0 {
			t.Fatal("fresh healthy telemetry did not establish recovery")
		}
	})
}
