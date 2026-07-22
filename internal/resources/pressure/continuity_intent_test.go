package pressure

import (
	"testing"
	"time"
)

func receiveContinuityIntent(t *testing.T, intents <-chan RestartContinuityIntent) RestartContinuityIntent {
	t.Helper()
	select {
	case intent := <-intents:
		return intent
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for restart-continuity intent")
		return RestartContinuityIntent{}
	}
}

func TestTaskGuardContinuityLateAttachmentReplaysWarning(t *testing.T) {
	f := newTaskGuardFixture(t)
	guard := NewTaskGuard(TaskGuardConfig{
		ProcRoot: f.procRoot, CgroupRoot: f.cgroupRoot, Admission: NewAdmissionGate(),
	})
	guard.SampleNow()
	f.write(t, 50, "100", "max 0\n")
	guard.SampleNow()
	if got := guard.SampleNow(); got.State != TaskPressureWarning {
		t.Fatalf("pressure state = %s, want warning", got.State)
	}

	intents := make(chan RestartContinuityIntent, 1)
	guard.AttachRestartContinuity(RestartContinuityCapabilityFunc(func(intent RestartContinuityIntent, _ RestartContinuityIntentView) {
		intents <- intent
	}))
	if got := receiveContinuityIntent(t, intents); got.State != TaskPressureWarning || got.Generation != 1 {
		t.Fatalf("replayed intent = %+v, want warning generation 1", got)
	}
}

func TestTaskGuardContinuityLateAttachmentReplaysOnlyNormalAfterWarning(t *testing.T) {
	f := newTaskGuardFixture(t)
	guard := NewTaskGuard(TaskGuardConfig{
		ProcRoot: f.procRoot, CgroupRoot: f.cgroupRoot, Admission: NewAdmissionGate(),
	})
	guard.SampleNow()
	f.write(t, 50, "100", "max 0\n")
	guard.SampleNow()
	guard.SampleNow()
	f.write(t, 39, "100", "max 0\n")
	guard.SampleNow()
	if got := guard.SampleNow(); got.State != TaskPressureNormal {
		t.Fatalf("pressure state = %s, want normal", got.State)
	}

	intents := make(chan RestartContinuityIntent, 2)
	guard.AttachRestartContinuity(RestartContinuityCapabilityFunc(func(intent RestartContinuityIntent, _ RestartContinuityIntentView) {
		intents <- intent
	}))
	if got := receiveContinuityIntent(t, intents); got.State != TaskPressureNormal || got.Generation != 2 {
		t.Fatalf("replayed intent = %+v, want normal generation 2", got)
	}
	select {
	case extra := <-intents:
		t.Fatalf("late attachment replayed stale intent: %+v", extra)
	default:
	}
}

func TestTaskGuardContinuityLateAttachmentReplaysOnlyCritical(t *testing.T) {
	f := newTaskGuardFixture(t)
	guard := NewTaskGuard(TaskGuardConfig{
		ProcRoot: f.procRoot, CgroupRoot: f.cgroupRoot, Admission: NewAdmissionGate(),
	})
	guard.SampleNow()
	f.write(t, 75, "100", "max 0\n")
	if got := guard.SampleNow(); got.State != TaskPressureCritical {
		t.Fatalf("pressure state = %s, want critical", got.State)
	}

	intents := make(chan RestartContinuityIntent, 1)
	guard.AttachRestartContinuity(RestartContinuityCapabilityFunc(func(intent RestartContinuityIntent, _ RestartContinuityIntentView) {
		intents <- intent
	}))
	if got := receiveContinuityIntent(t, intents); got.State != TaskPressureCritical || got.Generation != 1 {
		t.Fatalf("replayed intent = %+v, want critical generation 1", got)
	}
}

func TestTaskGuardContinuityLateCompletionRechecksLatestState(t *testing.T) {
	f := newTaskGuardFixture(t)
	critical := make(chan TaskSnapshot, 1)
	guard := NewTaskGuard(TaskGuardConfig{
		ProcRoot: f.procRoot, CgroupRoot: f.cgroupRoot,
		Admission: NewAdmissionGate(), CommitCritical: commitCriticalTo(critical),
	})
	started := make(chan RestartContinuityIntent, 2)
	releaseWarning := make(chan struct{})
	guard.AttachRestartContinuity(RestartContinuityCapabilityFunc(func(intent RestartContinuityIntent, _ RestartContinuityIntentView) {
		started <- intent
		if intent.State == TaskPressureWarning {
			<-releaseWarning
		}
	}))

	guard.SampleNow()
	f.write(t, 50, "100", "max 0\n")
	guard.SampleNow()
	guard.SampleNow()
	if got := receiveContinuityIntent(t, started); got.State != TaskPressureWarning || got.Generation != 1 {
		t.Fatalf("first intent = %+v, want warning generation 1", got)
	}

	// Advance through Normal to Critical while the Warning application is
	// still blocked. Neither sampling nor the Critical commit may wait for it.
	f.write(t, 39, "100", "max 0\n")
	guard.SampleNow()
	guard.SampleNow()
	f.write(t, 75, "100", "max 0\n")
	if got := guard.SampleNow(); got.State != TaskPressureCritical {
		t.Fatalf("pressure state = %s, want critical", got.State)
	}
	select {
	case <-critical:
	default:
		t.Fatal("Critical commit waited for blocked continuity callback")
	}
	select {
	case unexpected := <-started:
		t.Fatalf("callbacks overlapped instead of coalescing: %+v", unexpected)
	default:
	}

	close(releaseWarning)
	if got := receiveContinuityIntent(t, started); got.State != TaskPressureCritical || got.Generation != 3 {
		t.Fatalf("post-completion intent = %+v, want latest critical generation 3", got)
	}
}

func TestTaskGuardContinuityWarningViewAdvancesToNormalBeforeCallbackResumes(t *testing.T) {
	f := newTaskGuardFixture(t)
	guard := NewTaskGuard(TaskGuardConfig{
		ProcRoot: f.procRoot, CgroupRoot: f.cgroupRoot, Admission: NewAdmissionGate(),
	})
	warningStarted := make(chan bool, 1)
	checkLatest := make(chan struct{})
	observedLatest := make(chan RestartContinuityIntent, 1)
	observedCurrent := make(chan bool, 1)
	releaseWarning := make(chan struct{})
	normalApplied := make(chan struct{})
	guard.AttachRestartContinuity(RestartContinuityCapabilityFunc(func(
		intent RestartContinuityIntent,
		view RestartContinuityIntentView,
	) {
		switch intent.State {
		case TaskPressureWarning:
			warningStarted <- view.IsCurrent(intent)
			<-checkLatest
			observedLatest <- view.Latest()
			observedCurrent <- view.IsCurrent(intent)
			<-releaseWarning
		case TaskPressureNormal:
			close(normalApplied)
		}
	}))

	guard.SampleNow()
	f.write(t, 50, "100", "max 0\n")
	guard.SampleNow()
	guard.SampleNow()
	if !<-warningStarted {
		t.Fatal("Warning intent was not current when its callback began")
	}

	f.write(t, 39, "100", "max 0\n")
	guard.SampleNow()
	guard.SampleNow()
	checkLatest <- struct{}{}
	if got := <-observedLatest; got.State != TaskPressureNormal || got.Generation != 2 {
		t.Fatalf("latest intent while Warning callback blocked = %+v, want normal generation 2", got)
	}
	if <-observedCurrent {
		t.Fatal("blocked Warning callback still considered current after Normal transition")
	}

	close(releaseWarning)
	select {
	case <-normalApplied:
	case <-time.After(time.Second):
		t.Fatal("Normal intent was not applied after stale Warning callback returned")
	}
}

func TestTaskGuardContinuityNormalViewAdvancesToCriticalBeforeCallbackResumes(t *testing.T) {
	f := newTaskGuardFixture(t)
	critical := make(chan TaskSnapshot, 1)
	guard := NewTaskGuard(TaskGuardConfig{
		ProcRoot: f.procRoot, CgroupRoot: f.cgroupRoot,
		Admission: NewAdmissionGate(), CommitCritical: commitCriticalTo(critical),
	})
	warningApplied := make(chan struct{})
	normalStarted := make(chan bool, 1)
	checkLatest := make(chan struct{})
	observedLatest := make(chan RestartContinuityIntent, 1)
	observedCurrent := make(chan bool, 1)
	releaseNormal := make(chan struct{})
	criticalApplied := make(chan struct{})
	guard.AttachRestartContinuity(RestartContinuityCapabilityFunc(func(
		intent RestartContinuityIntent,
		view RestartContinuityIntentView,
	) {
		switch intent.State {
		case TaskPressureWarning:
			close(warningApplied)
		case TaskPressureNormal:
			normalStarted <- view.IsCurrent(intent)
			<-checkLatest
			observedLatest <- view.Latest()
			observedCurrent <- view.IsCurrent(intent)
			<-releaseNormal
		case TaskPressureCritical:
			close(criticalApplied)
		}
	}))

	guard.SampleNow()
	f.write(t, 50, "100", "max 0\n")
	guard.SampleNow()
	guard.SampleNow()
	select {
	case <-warningApplied:
	case <-time.After(time.Second):
		t.Fatal("Warning intent was not applied")
	}
	f.write(t, 39, "100", "max 0\n")
	guard.SampleNow()
	guard.SampleNow()
	if !<-normalStarted {
		t.Fatal("Normal intent was not current when its callback began")
	}

	f.write(t, 75, "100", "max 0\n")
	guard.SampleNow()
	select {
	case <-critical:
	default:
		t.Fatal("Critical emergency commit was blocked by Normal callback")
	}
	checkLatest <- struct{}{}
	if got := <-observedLatest; got.State != TaskPressureCritical || got.Generation != 3 {
		t.Fatalf("latest intent while Normal callback blocked = %+v, want critical generation 3", got)
	}
	if <-observedCurrent {
		t.Fatal("blocked Normal callback still considered current after Critical transition")
	}

	close(releaseNormal)
	select {
	case <-criticalApplied:
	case <-time.After(time.Second):
		t.Fatal("Critical intent was not applied after stale Normal callback returned")
	}
}

func TestTaskGuardContinuityCriticalCommitPrecedesNonBlockingCallback(t *testing.T) {
	f := newTaskGuardFixture(t)
	critical := make(chan TaskSnapshot, 1)
	callbackStarted := make(chan struct{})
	callbackRelease := make(chan struct{})
	ordered := make(chan bool, 1)
	guard := NewTaskGuard(TaskGuardConfig{
		ProcRoot: f.procRoot, CgroupRoot: f.cgroupRoot,
		Admission: NewAdmissionGate(), CommitCritical: commitCriticalTo(critical),
	})
	guard.AttachRestartContinuity(RestartContinuityCapabilityFunc(func(intent RestartContinuityIntent, _ RestartContinuityIntentView) {
		if intent.State != TaskPressureCritical {
			return
		}
		select {
		case <-critical:
			ordered <- true
		default:
			ordered <- false
		}
		close(callbackStarted)
		<-callbackRelease
	}))
	guard.SampleNow()
	f.write(t, 75, "100", "max 0\n")
	sampleDone := make(chan struct{})
	go func() {
		guard.SampleNow()
		close(sampleDone)
	}()

	select {
	case <-callbackStarted:
	case <-time.After(time.Second):
		t.Fatal("Critical continuity callback did not start")
	}
	if !<-ordered {
		t.Fatal("Critical continuity callback ran before emergency commit")
	}
	select {
	case <-sampleDone:
	case <-time.After(time.Second):
		t.Fatal("blocked Critical continuity callback delayed TaskGuard")
	}
	close(callbackRelease)
}

func TestTaskGuardContinuityRepeatedSamplesDoNotAdvanceGeneration(t *testing.T) {
	f := newTaskGuardFixture(t)
	guard := NewTaskGuard(TaskGuardConfig{
		ProcRoot: f.procRoot, CgroupRoot: f.cgroupRoot, Admission: NewAdmissionGate(),
	})
	intents := make(chan RestartContinuityIntent, 1)
	guard.AttachRestartContinuity(RestartContinuityCapabilityFunc(func(intent RestartContinuityIntent, _ RestartContinuityIntentView) {
		intents <- intent
	}))
	guard.SampleNow()
	f.write(t, 50, "100", "max 0\n")
	guard.SampleNow()
	guard.SampleNow()
	if got := receiveContinuityIntent(t, intents); got.State != TaskPressureWarning || got.Generation != 1 {
		t.Fatalf("intent = %+v, want warning generation 1", got)
	}
	guard.SampleNow()
	guard.SampleNow()

	guard.mu.RLock()
	generation := guard.continuityGeneration
	guard.mu.RUnlock()
	if generation != 1 {
		t.Fatalf("repeated Warning samples advanced generation to %d", generation)
	}
}
