package pressure

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"piccolod/internal/events"
	"piccolod/internal/health"
)

type taskGuardFixture struct {
	procRoot   string
	cgroupRoot string
	cgroupPath string
}

func newTaskGuardFixture(t *testing.T) taskGuardFixture {
	t.Helper()
	root := t.TempDir()
	procRoot := filepath.Join(root, "proc")
	cgroupRoot := filepath.Join(root, "cgroup")
	cgroupPath := filepath.Join(cgroupRoot, "system.slice", "piccolod.service")
	for _, dir := range []string{filepath.Join(procRoot, "self"), cgroupPath} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(procRoot, "self", "cgroup"), []byte("0::/system.slice/piccolod.service\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := taskGuardFixture{procRoot: procRoot, cgroupRoot: cgroupRoot, cgroupPath: cgroupPath}
	f.write(t, 10, "100", "max 0\n")
	return f
}

func (f taskGuardFixture) write(t *testing.T, current int64, limit, eventsValue string) {
	t.Helper()
	values := map[string]string{
		"pids.current": fmt.Sprintf("%d\n", current),
		"pids.max":     limit + "\n",
		"pids.events":  eventsValue,
		"cgroup.procs": "",
	}
	for name, value := range values {
		if err := os.WriteFile(filepath.Join(f.cgroupPath, name), []byte(value), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func commitCriticalTo(ch chan<- TaskSnapshot) func(TaskSnapshot) bool {
	return func(snapshot TaskSnapshot) bool {
		select {
		case ch <- snapshot:
			return true
		default:
			return false
		}
	}
}

func TestTaskGuardThresholdHysteresisAndCallbacks(t *testing.T) {
	f := newTaskGuardFixture(t)
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	gate := NewAdmissionGate()
	critical := make(chan TaskSnapshot, 1)
	closedDetached := make(chan struct{}, 1)
	resumed := 0
	guard := NewTaskGuard(TaskGuardConfig{
		ProcRoot:       f.procRoot,
		CgroupRoot:     f.cgroupRoot,
		Now:            func() time.Time { return now },
		Admission:      gate,
		CommitCritical: commitCriticalTo(critical),
		CloseDetached: func() {
			closedDetached <- struct{}{}
		},
		OnNormal: func() { resumed++ },
	})

	if got := guard.SampleNow(); got.State != TaskPressureNormal {
		t.Fatalf("initial state = %s", got.State)
	}
	f.write(t, 50, "100", "max 0\n")
	guard.SampleNow()
	if gate.Fenced() {
		t.Fatal("one high sample fenced admission")
	}
	got := guard.SampleNow()
	if got.State != TaskPressureWarning || !gate.Fenced() {
		t.Fatalf("warning transition = %+v fenced=%v", got, gate.Fenced())
	}
	select {
	case <-closedDetached:
	case <-time.After(time.Second):
		t.Fatal("warning transition did not request detached-session shedding")
	}
	f.write(t, 39, "100", "max 0\n")
	guard.SampleNow()
	got = guard.SampleNow()
	if got.State != TaskPressureNormal || gate.Fenced() || resumed != 1 {
		t.Fatalf("normal recovery = %+v fenced=%v resume_calls=%d", got, gate.Fenced(), resumed)
	}
	f.write(t, 75, "100", "max 0\n")
	got = guard.SampleNow()
	if got.State != TaskPressureCritical || got.ReasonCode != ReasonHighWater {
		t.Fatalf("critical transition = %+v", got)
	}
	select {
	case committed := <-critical:
		if committed != got {
			t.Fatalf("committed Critical snapshot = %+v, want %+v", committed, got)
		}
	default:
		t.Fatal("critical owner was not committed")
	}
}

type blockingLogWriter struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *blockingLogWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	return len(p), nil
}

func TestTaskGuardCommitsCriticalBeforeBlockedReporting(t *testing.T) {
	f := newTaskGuardFixture(t)
	bus := events.NewBus()
	_, cancel := bus.SubscribeWithCancel(events.TopicResourcePressure, 1)
	defer cancel()
	critical := make(chan TaskSnapshot, 1)
	guard := NewTaskGuard(TaskGuardConfig{
		ProcRoot: f.procRoot, CgroupRoot: f.cgroupRoot,
		Admission: NewAdmissionGate(), Bus: bus, CommitCritical: commitCriticalTo(critical),
	})
	guard.SampleNow() // fill the subscriber buffer so the next report logs a drop

	writer := &blockingLogWriter{started: make(chan struct{}), release: make(chan struct{})}
	previousWriter := log.Writer()
	log.SetOutput(writer)
	defer log.SetOutput(previousWriter)
	f.write(t, 75, "100", "max 0\n")
	done := make(chan struct{})
	go func() {
		guard.SampleNow()
		close(done)
	}()

	select {
	case <-critical:
	case <-time.After(time.Second):
		t.Fatal("Critical commit was delayed behind reporting")
	}
	select {
	case <-writer.started:
	case <-time.After(time.Second):
		t.Fatal("test did not reach the blocked reporting sink")
	}
	close(writer.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("critical sample did not finish after reporting was released")
	}
}

func TestTaskGuardBlockedWarningSheddingCannotStopCriticalSampling(t *testing.T) {
	f := newTaskGuardFixture(t)
	now := time.Now()
	critical := make(chan TaskSnapshot, 1)
	sheddingStarted := make(chan struct{})
	sheddingRelease := make(chan struct{})
	guard := NewTaskGuard(TaskGuardConfig{
		ProcRoot: f.procRoot, CgroupRoot: f.cgroupRoot,
		Now: func() time.Time { return now }, Admission: NewAdmissionGate(), CommitCritical: commitCriticalTo(critical),
		CloseDetached: func() {
			close(sheddingStarted)
			<-sheddingRelease
		},
	})
	guard.SampleNow()
	f.write(t, 55, "100", "max 0\n")
	guard.SampleNow()
	if got := guard.SampleNow(); got.State != TaskPressureWarning {
		t.Fatalf("warning sample = %+v", got)
	}
	select {
	case <-sheddingStarted:
	case <-time.After(time.Second):
		t.Fatal("warning shedding did not start")
	}

	now = now.Add(warningSustain + time.Second)
	if got := guard.SampleNow(); got.State != TaskPressureCritical {
		t.Fatalf("critical escalation = %+v", got)
	}
	select {
	case <-critical:
	case <-time.After(time.Second):
		t.Fatal("blocked warning shedding prevented Critical delivery")
	}
	close(sheddingRelease)
}

func TestTaskGuardSustainedWarningEscalates(t *testing.T) {
	f := newTaskGuardFixture(t)
	now := time.Now()
	guard := NewTaskGuard(TaskGuardConfig{
		ProcRoot: f.procRoot, CgroupRoot: f.cgroupRoot,
		Now: func() time.Time { return now }, Admission: NewAdmissionGate(),
	})
	guard.SampleNow()
	f.write(t, 55, "100", "max 0\n")
	guard.SampleNow()
	guard.SampleNow()
	now = now.Add(warningSustain + time.Second)
	got := guard.SampleNow()
	if got.State != TaskPressureCritical || got.ReasonCode != ReasonSustainedHighWater {
		t.Fatalf("sustained warning = %+v", got)
	}
}

func TestTaskGuardMaxEventUsesStartupBaseline(t *testing.T) {
	f := newTaskGuardFixture(t)
	f.write(t, 10, "100", "max 7\n")
	guard := NewTaskGuard(TaskGuardConfig{ProcRoot: f.procRoot, CgroupRoot: f.cgroupRoot, Admission: NewAdmissionGate()})
	if got := guard.SampleNow(); got.State != TaskPressureNormal {
		t.Fatalf("historical max event caused critical: %+v", got)
	}
	f.write(t, 10, "100", "max 8\n")
	got := guard.SampleNow()
	if got.State != TaskPressureCritical || got.ReasonCode != ReasonMaxEvent {
		t.Fatalf("max delta = %+v", got)
	}
}

func TestTaskGuardUnavailableDoesNotSignalCritical(t *testing.T) {
	f := newTaskGuardFixture(t)
	f.write(t, 10, "max", "max 0\n")
	tracker := health.NewTracker()
	critical := make(chan TaskSnapshot, 1)
	gate := NewAdmissionGate()
	guard := NewTaskGuard(TaskGuardConfig{
		ProcRoot: f.procRoot, CgroupRoot: f.cgroupRoot,
		Admission: gate, Health: tracker, CommitCritical: commitCriticalTo(critical),
	})
	got := guard.SampleNow()
	if got.State != TaskPressureUnavailable || got.ReasonCode != ReasonMonitorUnavailable {
		t.Fatalf("unavailable snapshot = %+v", got)
	}
	if status, ok := tracker.Status("task-pressure"); !ok || status.Level != health.LevelError {
		t.Fatalf("task-pressure health = %+v ok=%v", status, ok)
	}
	if !gate.Fenced() {
		t.Fatal("monitor-unavailable state did not fence admission")
	}
	f.write(t, 10, "100", "max 0\n")
	if got := guard.SampleNow(); got.State != TaskPressureNormal || gate.Fenced() {
		t.Fatalf("monitor recovery = %+v fenced=%v", got, gate.Fenced())
	}
	select {
	case snapshot := <-critical:
		t.Fatalf("false Critical commit: %+v", snapshot)
	default:
	}
}

func TestTaskGuardUnavailablePreservesBoundedCoreStartup(t *testing.T) {
	f := newTaskGuardFixture(t)
	f.write(t, 10, "max", "max 0\n")
	gate := NewAdmissionGate()
	gate.BeginCoreStartup()
	guard := NewTaskGuard(TaskGuardConfig{
		ProcRoot: f.procRoot, CgroupRoot: f.cgroupRoot, Admission: gate,
	})

	got := guard.SampleNow()
	if got.State != TaskPressureUnavailable {
		t.Fatalf("unavailable snapshot = %+v", got)
	}
	if err := gate.Check(context.Background(), WorkStorage); err != nil {
		t.Fatalf("core startup was fenced by monitor failure: %v", err)
	}
	gate.EndCoreStartup()
	if err := gate.Check(context.Background(), WorkStorage); !IsAdmissionError(err) {
		t.Fatalf("optional work admitted after core startup: %v", err)
	}
}

func TestTaskGuardNormalSampleDoesNotReleaseProcessFatalFence(t *testing.T) {
	f := newTaskGuardFixture(t)
	f.write(t, 10, "100", "max 0\n")
	gate := NewAdmissionGate()
	gate.FenceCritical()
	guard := NewTaskGuard(TaskGuardConfig{
		ProcRoot: f.procRoot, CgroupRoot: f.cgroupRoot, Admission: gate,
	})

	if got := guard.SampleNow(); got.State != TaskPressureNormal {
		t.Fatalf("normal snapshot = %+v", got)
	}
	if !gate.Fenced() {
		t.Fatal("normal task sample released the process-fatal admission fence")
	}
}

func TestTaskGuardDisabledDoesNotReleaseProcessFatalFence(t *testing.T) {
	gate := NewAdmissionGate()
	gate.FenceCritical()
	guard := NewTaskGuard(TaskGuardConfig{Disabled: true, Admission: gate})

	if err := guard.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := guard.Snapshot(); got.State != TaskPressureNormal || got.ReasonCode != ReasonNormal {
		t.Fatalf("disabled guard snapshot = %+v", got)
	}
	if !gate.Fenced() {
		t.Fatal("disabled guard released the process-fatal admission fence")
	}
}

func TestTaskGuardMalformedEventsKeepsThresholdMonitoringDegraded(t *testing.T) {
	f := newTaskGuardFixture(t)
	f.write(t, 10, "100", "broken\n")
	bus := events.NewBus()
	ch, cancel := bus.SubscribeWithCancel(events.TopicResourcePressure, 1)
	defer cancel()
	guard := NewTaskGuard(TaskGuardConfig{
		ProcRoot: f.procRoot, CgroupRoot: f.cgroupRoot,
		Admission: NewAdmissionGate(), Health: health.NewTracker(), Bus: bus,
	})
	got := guard.SampleNow()
	if got.State != TaskPressureUnavailable || !got.CurrentKnown || !got.LimitKnown {
		t.Fatalf("malformed events snapshot = %+v", got)
	}
	if !got.AllowsAutomaticRecovery() {
		t.Fatal("malformed events disabled recovery despite known headroom")
	}
	select {
	case ev := <-ch:
		payload := ev.Payload.(events.ResourcePressureEvent)
		if payload.Severity != events.PressureSeverityWarn || payload.ReasonCode != ReasonMonitorUnavailable {
			t.Fatalf("event payload = %+v", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("missing degraded event")
	}
}

func TestTaskGuardMalformedEventsLowSampleReleasesUnavailableAdmission(t *testing.T) {
	f := newTaskGuardFixture(t)
	f.write(t, 10, "max", "max 0\n")
	gate := NewAdmissionGate()
	guard := NewTaskGuard(TaskGuardConfig{
		ProcRoot: f.procRoot, CgroupRoot: f.cgroupRoot, Admission: gate,
	})

	if got := guard.SampleNow(); got.State != TaskPressureUnavailable || !gate.Fenced() {
		t.Fatalf("unavailable sample = %+v fenced=%t", got, gate.Fenced())
	}
	f.write(t, 10, "100", "broken\n")
	got := guard.SampleNow()
	if got.State != TaskPressureUnavailable || !got.AllowsAutomaticRecovery() {
		t.Fatalf("malformed-events recovery sample = %+v", got)
	}
	if gate.Fenced() {
		t.Fatal("known low headroom retained the monitor-unavailable admission fence")
	}
}

func TestTaskSnapshotAutomaticRecoveryRequiresKnownHeadroom(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   TaskSnapshot
		want bool
	}{
		{name: "normal", in: TaskSnapshot{State: TaskPressureNormal}, want: true},
		{
			name: "events unavailable below warning",
			in: TaskSnapshot{
				State: TaskPressureUnavailable, ReasonCode: ReasonMonitorUnavailable,
				Current: 49, Limit: 100, CurrentKnown: true, LimitKnown: true,
			},
			want: true,
		},
		{
			name: "events unavailable at warning threshold",
			in: TaskSnapshot{
				State: TaskPressureUnavailable, ReasonCode: ReasonMonitorUnavailable,
				Current: 50, Limit: 100, CurrentKnown: true, LimitKnown: true,
			},
		},
		{
			name: "unknown current",
			in: TaskSnapshot{
				State: TaskPressureUnavailable, ReasonCode: ReasonMonitorUnavailable,
				Limit: 100, LimitKnown: true,
			},
		},
		{
			name: "warning",
			in: TaskSnapshot{
				State: TaskPressureWarning, Current: 60, Limit: 100,
				CurrentKnown: true, LimitKnown: true,
			},
		},
		{name: "critical", in: TaskSnapshot{State: TaskPressureCritical}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.AllowsAutomaticRecovery(); got != tc.want {
				t.Fatalf("AllowsAutomaticRecovery() = %t; want %t for %+v", got, tc.want, tc.in)
			}
		})
	}
}

func TestResolveServiceCgroupRejectsSymlinkEscape(t *testing.T) {
	f := newTaskGuardFixture(t)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(f.cgroupRoot, "system.slice")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(f.cgroupRoot, "system.slice")); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveServiceCgroup(f.procRoot, f.cgroupRoot); err == nil {
		t.Fatal("symlink escape was accepted")
	}
}

func TestTaskGuardStartStopOwnsOneLoop(t *testing.T) {
	f := newTaskGuardFixture(t)
	guard := NewTaskGuard(TaskGuardConfig{
		ProcRoot: f.procRoot, CgroupRoot: f.cgroupRoot,
		Interval: time.Millisecond, Admission: NewAdmissionGate(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := guard.Start(ctx); err != nil {
		t.Fatal(err)
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	defer stopCancel()
	if err := guard.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
}
